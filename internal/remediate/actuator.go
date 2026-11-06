package remediate

import (
	"context"
	"errors"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/haproxy"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	defaultDrainWait    = 5 * time.Second
)

type Actuator interface {
	Drain(ctx context.Context, host string) error
	Restore(ctx context.Context, host string) error
}

// HAProxyActuator acts through the runtime API. The poll settings are only set
// by tests.
type HAProxyActuator struct {
	C            *haproxy.Client
	Backend      string
	pollInterval time.Duration
	drainWait    time.Duration
}

// Drain lets in-flight requests finish, up to a limit, then takes the server out
// fully. If the last step fails the server is put back: the planner will not
// count a failed drain, so a half-drained server would escape the cap. Only the
// wait for idle stops when ctx ends: a daemon shutting down must not leave a server
// half drained, and cancelling a command HAProxy has already run would lose its
// reply and report a failure. The client's own timeout bounds these calls.
func (a *HAProxyActuator) Drain(ctx context.Context, host string) error {
	finish := context.WithoutCancel(ctx)
	if err := a.C.SetState(finish, a.Backend, host, haproxy.Drain); err != nil {
		return err
	}
	a.waitForIdle(ctx, host)
	if err := a.C.SetState(finish, a.Backend, host, haproxy.Maint); err != nil {
		return errors.Join(err, a.C.SetState(finish, a.Backend, host, haproxy.Ready))
	}
	return nil
}

func (a *HAProxyActuator) Restore(ctx context.Context, host string) error {
	return a.C.SetState(ctx, a.Backend, host, haproxy.Ready)
}

// waitForIdle returns once the server has no connections, the limit passes, or
// ctx ends. A failed poll just means waiting longer; the limit still applies.
func (a *HAProxyActuator) waitForIdle(ctx context.Context, host string) {
	deadline := time.Now().Add(a.waitLimit())
	interval := a.pollInterval
	if interval == 0 {
		interval = defaultPollInterval
	}
	for {
		if stats, err := a.C.ShowStat(ctx, a.Backend); err == nil {
			for _, s := range stats {
				if s.Name == host && s.Scur == 0 {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (a *HAProxyActuator) waitLimit() time.Duration {
	if a.drainWait > 0 {
		return a.drainWait
	}
	return defaultDrainWait
}
