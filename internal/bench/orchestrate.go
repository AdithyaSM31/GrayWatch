package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

// cleanupTimeout bounds the work done after an interrupt. It has to outlast a
// revert on every host, and must still end if something is stuck.
const cleanupTimeout = 2 * time.Minute

// Config is one way of running the fleet: which health check HAProxy uses, and
// whether Graywatch only reports or also drains.
type Config struct{ Health, Mode string }

func (c Config) String() string { return c.Health + ":" + c.Mode }

// ParseConfigs reads a list such as "shallow:detect,deep:detect".
func ParseConfigs(s string) ([]Config, error) {
	var out []Config
	for _, part := range strings.Split(s, ",") {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) != 2 {
			return nil, fmt.Errorf("config %q is not health:mode", part)
		}
		health, mode := fields[0], fields[1]
		if health != "shallow" && health != "deep" {
			return nil, fmt.Errorf("config %q: health must be shallow or deep", part)
		}
		if mode != "detect" && mode != "remediate" {
			return nil, fmt.Errorf("config %q: mode must be detect or remediate", part)
		}
		out = append(out, Config{Health: health, Mode: mode})
	}
	return out, nil
}

// Daemon is a running graywatchd. Stop returns an error if it had already exited,
// because a trial it missed would otherwise score as a clean miss.
type Daemon interface{ Stop() error }

// World is everything a run does to the machine it runs on.
type World interface {
	FleetUp(ctx context.Context, health string) error
	WaitReady(ctx context.Context) error
	CheckMapping(ctx context.Context) error
	Hosts(ctx context.Context) ([]string, error)
	StartDaemon(ctx context.Context, eventsPath string, dryRun bool) (Daemon, error)
	Apply(ctx context.Context, fault, host string) error
	Revert(ctx context.Context, fault, host string) error
	RevertAll(ctx context.Context) error
	SetAllReady(ctx context.Context) error
	Servers(ctx context.Context) ([]haproxy.ServerStat, error)
	Load(ctx context.Context, out chan<- loadgen.Result) error
	ReadEvents(path string) ([]events.Event, error)
}

type Orchestrator struct {
	World     World
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
	Save      func([]TrialResult) error
	Logf      func(format string, args ...any)
	OutDir    string
	Seed      uint64
	Warmup    time.Duration
	PollEvery time.Duration

	results []TrialResult
}

// Run runs every scenario under every config and returns the finished trials,
// which are also saved after each one. A run that fails or is interrupted reverts
// its faults and returns what it has along with the error.
func (o *Orchestrator) Run(ctx context.Context, scs []Scenario, cfgs []Config, trials int) ([]TrialResult, error) {
	for _, cfg := range cfgs {
		if err := o.runConfig(ctx, scs, cfg, trials); err != nil {
			return o.results, fmt.Errorf("%s: %w", cfg, err)
		}
	}
	return o.results, nil
}

// EstimateDuration is how long a run takes, not counting fleet starts.
func EstimateDuration(scs []Scenario, configs, trials int, warmup time.Duration) time.Duration {
	var perConfig time.Duration
	for _, sc := range scs {
		n := trials
		if sc.Target == TargetNone {
			n = 1
		}
		perConfig += time.Duration(n) * (sc.Baseline + sc.FaultDur + sc.Cooldown)
	}
	return time.Duration(configs) * (warmup + perConfig)
}

// configRun is what must be undone when a config ends, however it ends.
type configRun struct {
	daemon     Daemon
	stopTasks  func()
	loadErr    error
	recorder   *trialRecorder
	eventsPath string
}

func (o *Orchestrator) runConfig(ctx context.Context, scs []Scenario, cfg Config, trials int) (err error) {
	run := &configRun{
		recorder:   &trialRecorder{},
		eventsPath: filepath.Join(o.OutDir, "events-"+cfg.Health+"-"+cfg.Mode+".jsonl"),
	}
	defer func() { err = errors.Join(err, o.cleanup(ctx, run)) }()

	// A crash in an earlier run can leave faults behind.
	if err := o.World.RevertAll(ctx); err != nil {
		return err
	}
	if err := o.World.FleetUp(ctx, cfg.Health); err != nil {
		return err
	}
	if err := o.World.WaitReady(ctx); err != nil {
		return err
	}
	if err := o.World.CheckMapping(ctx); err != nil {
		return err
	}
	hosts, err := o.World.Hosts(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return errors.New("no app hosts found")
	}

	o.startTasks(ctx, run)
	o.Logf("%s: warming up for %v", cfg, o.Warmup)
	if err := o.Sleep(ctx, o.Warmup); err != nil {
		return err
	}

	// Each config gets the same sequence of targets, so that they differ in
	// nothing but the config.
	rng := rand.New(rand.NewPCG(o.Seed, 0))
	for _, sc := range scs {
		n := trials
		if sc.Target == TargetNone {
			n = 1 // one long window: its result is a rate per host-hour
		}
		for trial := 1; trial <= n; trial++ {
			target := pickTarget(sc, hosts, rng)
			r, err := o.runTrial(ctx, run, sc, cfg, target)
			if err != nil {
				return fmt.Errorf("%s trial %d on %q: %w", sc.Name, trial, target, err)
			}
			r.Trial, r.Mode, r.Health = trial, cfg.Mode, cfg.Health
			o.results = append(o.results, r)
			if err := o.Save(o.results); err != nil {
				return err
			}
			o.Logf("%s %s trial %d on %q: graywatch %v in %v (%s), health check %v in %v, p99 %v, errors %.2f%%",
				cfg, sc.Name, trial, target, r.GWDetected, r.GWTTD, r.GWCategory, r.HCDetected, r.HCTTD, r.P99, r.ErrRate*100)
		}
	}
	return nil
}

func pickTarget(sc Scenario, hosts []string, rng *rand.Rand) string {
	switch sc.Target {
	case TargetDep:
		return "dep"
	case TargetNone:
		return ""
	default:
		return hosts[rng.IntN(len(hosts))]
	}
}

// runTrial starts its own graywatchd so that one trial's drains, cooldowns and
// flap counts cannot change the next one's result. The daemon is stopped before
// servers are set back to ready, since it would otherwise still count them as
// drained.
func (o *Orchestrator) runTrial(ctx context.Context, run *configRun, sc Scenario, cfg Config, target string) (TrialResult, error) {
	d, err := o.World.StartDaemon(ctx, run.eventsPath, cfg.Mode == "detect")
	if err != nil {
		return TrialResult{}, err
	}
	run.daemon = d
	run.recorder.take()

	tBase := o.Now()
	if err := o.Sleep(ctx, sc.Baseline); err != nil {
		return TrialResult{}, err
	}
	t0 := o.Now()
	if sc.Fault != "" {
		if err := o.World.Apply(ctx, sc.Fault, target); err != nil {
			return TrialResult{}, err
		}
	}
	if err := o.Sleep(ctx, sc.FaultDur); err != nil {
		return TrialResult{}, err
	}
	t1 := o.Now()
	if sc.Fault != "" {
		if err := o.World.Revert(ctx, sc.Fault, target); err != nil {
			return TrialResult{}, err
		}
	}
	if err := o.Sleep(ctx, sc.Cooldown); err != nil {
		return TrialResult{}, err
	}

	run.daemon = nil
	if err := d.Stop(); err != nil {
		return TrialResult{}, err
	}
	if err := o.World.SetAllReady(ctx); err != nil {
		return TrialResult{}, err
	}
	if err := o.World.WaitReady(ctx); err != nil {
		return TrialResult{}, err
	}

	reqs, hc := run.recorder.take()
	if len(reqs) == 0 {
		return TrialResult{}, errors.New("no requests were recorded")
	}
	evs, err := o.World.ReadEvents(run.eventsPath)
	if err != nil {
		return TrialResult{}, err
	}
	return Score(sc, target, tBase, t0, t1, evs, hc, reqs), nil
}

// startTasks runs the load and the health check poller for the whole config.
func (o *Orchestrator) startTasks(ctx context.Context, run *configRun) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	results := make(chan loadgen.Result)

	wg.Add(3)
	go func() {
		defer wg.Done()
		run.loadErr = o.World.Load(ctx, results)
	}()
	go func() {
		defer wg.Done()
		for r := range results {
			run.recorder.addRequest(r)
		}
	}()
	go func() {
		defer wg.Done()
		o.poll(ctx, run.recorder)
	}()
	run.stopTasks = func() {
		cancel()
		wg.Wait()
	}
}

func (o *Orchestrator) poll(ctx context.Context, rec *trialRecorder) {
	tick := time.NewTicker(o.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			stats, err := o.World.Servers(ctx)
			if err != nil {
				if ctx.Err() == nil {
					o.Logf("reading health checks: %v", err)
				}
				continue
			}
			rec.observe(o.Now(), stats)
		}
	}
}

// cleanup leaves the fleet clean. It uses its own context because the run's may
// be what was cancelled, and keeps going past errors. The order is the plan's: end
// the faults first, then put hosts back, then stop the daemon so it does not act
// on the hosts in between, and save last.
func (o *Orchestrator) cleanup(ctx context.Context, run *configRun) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	errs := []error{o.World.RevertAll(ctx), o.World.SetAllReady(ctx)}
	if run.daemon != nil {
		errs = append(errs, run.daemon.Stop())
		run.daemon = nil
		// A drain in flight above can land after the first pass.
		errs = append(errs, o.World.SetAllReady(ctx))
	}
	if run.stopTasks != nil {
		run.stopTasks()
		errs = append(errs, run.loadErr)
	}
	errs = append(errs, o.Save(o.results))
	return errors.Join(errs...)
}

// trialRecorder collects one trial's requests and health check readings. It keeps
// only the current trial's, as a full run sends millions of requests.
type trialRecorder struct {
	mu   sync.Mutex
	reqs []loadgen.Result
	hc   []HCObs
}

func (r *trialRecorder) addRequest(q loadgen.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, q)
}

func (r *trialRecorder) observe(at time.Time, stats []haproxy.ServerStat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range stats {
		if s.Down() {
			r.hc = append(r.hc, HCObs{At: at, Server: s.Name, Down: true})
		}
	}
}

// take returns what has been recorded and starts over.
func (r *trialRecorder) take() ([]loadgen.Result, []HCObs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs, hc := r.reqs, r.hc
	r.reqs, r.hc = nil, nil
	return reqs, hc
}
