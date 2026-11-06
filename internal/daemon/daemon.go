// Package daemon is graywatchd's main loop, kept out of cmd so it can be tested.
package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/detect"
	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/metrics"
	"github.com/AdithyaSM31/graywatch/internal/remediate"
)

const (
	rediscoverEvery = 30 * time.Second

	// trafficGap is how long the fleet must be silent before new traffic counts as
	// starting over. Containers recreated under a running daemon take about this
	// long, and they are cold again.
	trafficGap = 30 * time.Second

	// discoverTimeout bounds a docker call. dockerd can stall under the same disk or
	// memory trouble this tool looks for, and the daemon must keep ticking.
	discoverTimeout = 10 * time.Second
)

type Deps struct {
	Discover  func(ctx context.Context) ([]fleet.Host, error)
	Collector *collect.Collector
	Detector  *detect.Detector
	Planner   *remediate.Planner
	Actuator  remediate.Actuator
	Events    *events.Writer
	Metrics   *metrics.Metrics
	Now       func() time.Time

	// StartupGrace holds drains for this long after traffic starts. A host's first
	// requests are slow on every host, by up to 1130 ms for 2 s on a real VM, so the
	// slowest one looks sick. The signal window and debounce carry that to about
	// 10 s. Zero means no grace period.
	StartupGrace time.Duration

	// Servers reads HAProxy's view of the backend each tick, so the planner's
	// limits count what is really out of rotation. Nil means it isn't read.
	Servers func(ctx context.Context) ([]haproxy.ServerStat, error)
}

type Daemon struct {
	Deps
	lastDiscover time.Time
	states       map[string]detect.HostState
	repeating    map[string]bool
	trafficSince time.Time
	lastTraffic  time.Time

	serversFailing bool
}

func New(deps Deps) *Daemon {
	return &Daemon{Deps: deps, states: map[string]detect.HostState{}, repeating: map[string]bool{}}
}

// DrainedServers returns the servers HAProxy already has out of rotation.
func DrainedServers(stats []haproxy.ServerStat) []string {
	var drained []string
	for _, s := range stats {
		if strings.HasPrefix(s.Status, "DRAIN") || strings.HasPrefix(s.Status, "MAINT") {
			drained = append(drained, s.Name)
		}
	}
	return drained
}

// Step runs one tick. Drained hosts stay out of rotation if the daemon stops.
func (d *Daemon) Step(ctx context.Context) {
	d.rediscover(ctx)
	samples := d.Collector.Tick()
	if len(samples) == 0 {
		return
	}
	d.reconcile(ctx)
	obs := d.Detector.Observe(samples, d.Planner.Drained())
	d.Metrics.Record(samples, obs)
	d.emitTransitions(obs)
	d.noteTraffic(samples, obs.At)
	d.act(ctx, obs)
}

// reconcile keeps the last reading if this one fails, and logs only when the read
// starts failing, as it would otherwise repeat every second.
func (d *Daemon) reconcile(ctx context.Context) {
	if d.Servers == nil {
		return
	}
	stats, err := d.Servers(ctx)
	if err != nil {
		if !d.serversFailing && ctx.Err() == nil {
			log.Printf("reading HAProxy: %v", err)
		}
		d.serversFailing = true
		return
	}
	d.serversFailing = false
	d.Planner.Reconcile(stats)
}

// noteTraffic tracks when the fleet started taking traffic. Latency is reported
// only for a host with recent requests, so it tells whether traffic is flowing.
func (d *Daemon) noteTraffic(samples []collect.Sample, at time.Time) {
	withTraffic := 0
	for _, s := range samples {
		if _, ok := s.Values[collect.SigLatP90]; ok {
			withTraffic++
		}
	}
	if withTraffic*2 < len(samples) {
		return
	}
	if d.lastTraffic.IsZero() || at.Sub(d.lastTraffic) > trafficGap {
		d.trafficSince = at
	}
	d.lastTraffic = at
}

func (d *Daemon) inStartupGrace(at time.Time) bool {
	return d.StartupGrace > 0 && !d.trafficSince.IsZero() && at.Sub(d.trafficSince) < d.StartupGrace
}

// rediscover keeps the current hosts on a failed or empty result. A docker
// hiccup must not read as the whole fleet leaving.
func (d *Daemon) rediscover(ctx context.Context) {
	now := d.Now()
	if !d.lastDiscover.IsZero() && now.Sub(d.lastDiscover) < rediscoverEvery {
		return
	}
	d.lastDiscover = now
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	hosts, err := d.Discover(ctx)
	switch {
	case err != nil:
		log.Printf("discover: %v", err)
	case len(hosts) == 0:
		log.Print("discover: no hosts found")
	default:
		d.Collector.SetHosts(hosts)
	}
}

func (d *Daemon) emitTransitions(obs detect.Observation) {
	states := make(map[string]detect.HostState, len(obs.Verdicts))
	for _, v := range obs.Verdicts {
		states[v.Host] = v.State
		prev, known := d.states[v.Host]
		if !known {
			prev = detect.Healthy
		}
		if v.State == prev {
			continue
		}
		e := describe(events.Event{TS: obs.At, Host: v.Host}, v)
		if v.State == detect.Suspect {
			e.Type = events.Suspect
		} else {
			e.Type = events.Healthy
		}
		d.emit(e)
	}
	d.states = states
}

func (d *Daemon) act(ctx context.Context, obs detect.Observation) {
	repeating := map[string]bool{}
	for _, a := range d.Planner.Plan(obs) {
		e := eventFor(a, obs)
		if a.Kind == remediate.Drain && d.inStartupGrace(obs.At) {
			// Not committed, so the planner proposes it again once the grace ends.
			key := events.DrainHeld + "/" + a.Host
			repeating[key] = true
			if !d.repeating[key] {
				e.Type = events.DrainHeld
				e.Reason = fmt.Sprintf("%s, held by the %v startup grace", a.Reason, d.StartupGrace)
				d.emit(e)
			}
			continue
		}
		if a.DryRun || (a.Kind != remediate.Drain && a.Kind != remediate.Restore) {
			// The same alert or dry-run action comes back every tick while the
			// condition lasts. Report it when it starts, not once a second.
			key := string(a.Kind) + "/" + a.Host
			repeating[key] = true
			d.Planner.Commit(a, obs.At, nil)
			if !d.repeating[key] {
				d.emit(e)
				d.Metrics.CountAction(string(a.Kind))
			}
			continue
		}
		err := d.execute(ctx, a)
		d.Planner.Commit(a, obs.At, err)
		if err != nil {
			e.Type, e.Reason = events.ActionError, string(a.Kind)+": "+err.Error()
			d.emit(e)
			d.Metrics.CountAction(events.ActionError)
			continue
		}
		d.emit(e)
		d.Metrics.CountAction(string(a.Kind))
	}
	d.repeating = repeating
}

func (d *Daemon) execute(ctx context.Context, a remediate.Action) error {
	if a.Kind == remediate.Drain {
		return d.Actuator.Drain(ctx, a.Host)
	}
	return d.Actuator.Restore(ctx, a.Host)
}

func eventFor(a remediate.Action, obs detect.Observation) events.Event {
	e := events.Event{TS: obs.At, Host: a.Host, Reason: a.Reason, DryRun: a.DryRun}
	switch a.Kind {
	case remediate.Drain:
		e.Type = events.Drain
	case remediate.Restore:
		e.Type = events.Restore
	case remediate.AlertFleetWide:
		e.Type = events.FleetAlert
	case remediate.AlertCapReached:
		e.Type = events.CapReached
	case remediate.AlertFlapping:
		e.Type = events.Flapping
	}
	for _, v := range obs.Verdicts {
		if v.Host == a.Host {
			return describe(e, v)
		}
	}
	return e
}

func describe(e events.Event, v detect.Verdict) events.Event {
	e.Category, e.Score = v.Category, v.Score
	for _, s := range v.Signals {
		e.Signals = append(e.Signals, string(s))
	}
	return e
}

func (d *Daemon) emit(e events.Event) {
	if err := d.Events.Emit(e); err != nil {
		log.Printf("events: %v", err)
	}
}
