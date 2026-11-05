package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/chaos"
	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/fleetgen"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

const (
	readyTimeout    = 2 * time.Minute
	daemonStopWait  = 10 * time.Second
	ioThrottleFault = "io-throttle"
	requestTimeout  = 2 * time.Second
)

// AllUp reports whether n servers are all passing their health checks. HAProxy
// writes "UP 1/3" while it counts a server's rises, and that is still up.
func AllUp(stats []haproxy.ServerStat, n int) bool {
	if len(stats) != n {
		return false
	}
	for _, s := range stats {
		if s.Status != "UP" && !strings.HasPrefix(s.Status, "UP ") {
			return false
		}
	}
	return true
}

// CheckServerMapping fails if any HAProxy server is not sending traffic to the
// container it is named after. HAProxy resolves names only when it starts, so
// recreating containers without it swaps their addresses around. Every server
// still reads UP, and drains and per-host results land on the wrong host.
func CheckServerMapping(addrs map[string]string, hosts []fleet.Host) error {
	var wrong []string
	for _, h := range hosts {
		addr, ok := addrs[h.Name]
		switch {
		case !ok:
			wrong = append(wrong, h.Name+" has no HAProxy server")
		case addr != h.IP:
			wrong = append(wrong, fmt.Sprintf("%s goes to %s, but the container is at %s", h.Name, addr, h.IP))
		}
	}
	for name := range addrs {
		if !slices.ContainsFunc(hosts, func(h fleet.Host) bool { return h.Name == name }) {
			wrong = append(wrong, "server "+name+" has no container")
		}
	}
	if len(wrong) > 0 {
		slices.Sort(wrong)
		return fmt.Errorf("HAProxy does not match the fleet: %s", strings.Join(wrong, "; "))
	}
	return nil
}

// RealWorld runs against Docker, HAProxy and the kernel on this machine.
type RealWorld struct {
	DeployDir  string
	DaemonPath string
	FleetSize  int
	Backend    string
	HAProxy    *haproxy.Client
	Chaos      chaos.Env
	LoadConfig loadgen.Config
}

func (w *RealWorld) FleetUp(ctx context.Context, health string) error {
	compose, cfg, err := fleetgen.Render(w.FleetSize, health)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.DeployDir, 0o755); err != nil {
		return err
	}
	composePath := filepath.Join(w.DeployDir, "docker-compose.yml")
	if err := os.WriteFile(composePath, compose, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.DeployDir, "haproxy.cfg"), cfg, 0o644); err != nil {
		return err
	}
	// --force-recreate takes HAProxy along with the apps; see CheckServerMapping.
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "up", "-d", "--build", "--force-recreate")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose up: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *RealWorld) WaitReady(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		stats, err := w.Servers(ctx)
		if err == nil && AllUp(stats, w.FleetSize) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("servers not all up after %v: %v (%v)", readyTimeout, stats, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (w *RealWorld) discover(ctx context.Context) ([]fleet.Host, error) {
	return fleet.Discover(ctx, fleet.CLIDocker{}, "/proc")
}

func (w *RealWorld) CheckMapping(ctx context.Context) error {
	addrs, err := w.HAProxy.ServerAddrs(ctx, w.Backend)
	if err != nil {
		return err
	}
	hosts, err := w.discover(ctx)
	if err != nil {
		return err
	}
	return CheckServerMapping(addrs, hosts)
}

func (w *RealWorld) Hosts(ctx context.Context) ([]string, error) {
	hosts, err := w.discover(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return names, nil
}

func (w *RealWorld) Apply(ctx context.Context, faultName, host string) error {
	f, ok := chaos.Registry()[faultName]
	if !ok {
		return fmt.Errorf("unknown fault %q", faultName)
	}
	// The data directory only exists once the fleet has started.
	if faultName == ioThrottleFault && w.Chaos.IODevice == "" {
		device, err := cgroup.WholeDiskDevice("/sys", filepath.Join(w.DeployDir, "data"))
		if err != nil {
			return err
		}
		w.Chaos.IODevice = device
	}
	h, err := fleet.Lookup(ctx, fleet.CLIDocker{}, "/proc", host)
	if err != nil {
		return err
	}
	return chaos.ApplyWithState(ctx, w.Chaos, f, h)
}

func (w *RealWorld) Revert(ctx context.Context, faultName, host string) error {
	return chaos.RevertOne(ctx, w.Chaos, host, faultName)
}

func (w *RealWorld) RevertAll(ctx context.Context) error {
	return chaos.RevertAll(ctx, w.Chaos)
}

func (w *RealWorld) SetAllReady(ctx context.Context) error {
	stats, err := w.Servers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range stats {
		errs = append(errs, w.HAProxy.SetState(ctx, w.Backend, s.Name, haproxy.Ready))
	}
	return errors.Join(errs...)
}

func (w *RealWorld) Servers(ctx context.Context) ([]haproxy.ServerStat, error) {
	return w.HAProxy.ShowStat(ctx, w.Backend)
}

func (w *RealWorld) Load(ctx context.Context, out chan<- loadgen.Result) error {
	return loadgen.Run(ctx, w.LoadConfig, out)
}

func (w *RealWorld) ReadEvents(path string) ([]events.Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // the daemon writes nothing until it has something to say
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return events.ReadAll(f)
}

func (w *RealWorld) StartDaemon(_ context.Context, eventsPath string, dryRun bool) (Daemon, error) {
	logPath := strings.TrimSuffix(eventsPath, filepath.Ext(eventsPath)) + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	args := []string{"-haproxy", w.HAProxy.Addr, "-backend", w.Backend, "-events", eventsPath}
	if dryRun {
		args = append(args, "-dry-run")
	}
	// Not CommandContext: an interrupt has to find the daemon still running, so
	// that it is stopped after the faults end rather than before.
	cmd := exec.Command(w.DaemonPath, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detachFromTerminalSignals(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	p := &daemonProcess{cmd: cmd, done: make(chan error, 1)}
	go func() {
		p.done <- cmd.Wait()
		logFile.Close()
	}()
	return p, nil
}

type daemonProcess struct {
	cmd  *exec.Cmd
	done chan error
}

func (p *daemonProcess) Stop() error {
	select {
	case err := <-p.done:
		return fmt.Errorf("graywatchd had already exited: %v", err)
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case err := <-p.done:
		return err
	case <-time.After(daemonStopWait):
		p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("graywatchd did not stop within %v", daemonStopWait)
	}
}
