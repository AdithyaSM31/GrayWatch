package bench

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

// fakeWorld records what the orchestrator does. Its clock moves only when the
// orchestrator sleeps, so a four hour benchmark runs in milliseconds.
type fakeWorld struct {
	mu     sync.Mutex
	calls  []string
	now    time.Time
	cancel context.CancelFunc

	hosts      []string
	evs        []events.Event
	failOn     map[string]error // call name -> error
	cancelOn   string           // call name that cancels the run
	cancelWait time.Duration    // Sleep duration that cancels the run
	daemonDied bool
	noLoad     bool
	ctxDead    []string // calls made with a dead context
	sent       atomic.Int64
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{now: t0, hosts: []string{"app-01", "app-02", "app-03", "app-04"}, failOn: map[string]error{}}
}

func (w *fakeWorld) record(ctx context.Context, call string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, call)
	if ctx != nil && ctx.Err() != nil {
		w.ctxDead = append(w.ctxDead, call)
	}
	name, _, _ := strings.Cut(call, " ")
	if w.cancelOn == name && w.cancel != nil {
		w.cancel()
	}
	return w.failOn[name]
}

func (w *fakeWorld) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *fakeWorld) Sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.calls = append(w.calls, "sleep "+d.String())
	cancels := w.cancelWait == d && w.cancel != nil
	w.mu.Unlock()
	if cancels {
		w.cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.waitForTraffic()
	w.mu.Lock()
	w.now = w.now.Add(d)
	w.mu.Unlock()
	return nil
}

// waitForTraffic lets some requests through on each sleep. The channel is
// unbuffered, so once the next send completes the one before it has been recorded.
func (w *fakeWorld) waitForTraffic() {
	if w.noLoad {
		return
	}
	start := w.sent.Load()
	deadline := time.Now().Add(5 * time.Second)
	for w.sent.Load() < start+3 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

func (w *fakeWorld) FleetUp(ctx context.Context, health string) error {
	return w.record(ctx, "up "+health)
}
func (w *fakeWorld) WaitReady(ctx context.Context) error { return w.record(ctx, "ready") }
func (w *fakeWorld) CheckMapping(ctx context.Context) error {
	return w.record(ctx, "mapping")
}
func (w *fakeWorld) Hosts(ctx context.Context) ([]string, error) { return w.hosts, nil }
func (w *fakeWorld) Apply(ctx context.Context, fault, host string) error {
	return w.record(ctx, "apply "+fault+" "+host)
}
func (w *fakeWorld) Revert(ctx context.Context, fault, host string) error {
	return w.record(ctx, "revert "+fault+" "+host)
}
func (w *fakeWorld) RevertAll(ctx context.Context) error   { return w.record(ctx, "revert-all") }
func (w *fakeWorld) SetAllReady(ctx context.Context) error { return w.record(ctx, "set-ready") }
func (w *fakeWorld) Servers(ctx context.Context) ([]haproxy.ServerStat, error) {
	return nil, nil
}
func (w *fakeWorld) ReadEvents(path string) ([]events.Event, error) {
	w.record(nil, "read-events "+path)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.evs, nil
}

func (w *fakeWorld) Load(ctx context.Context, out chan<- loadgen.Result) error {
	defer close(out)
	if w.noLoad {
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case out <- loadgen.Result{Scheduled: w.Now(), Latency: time.Millisecond, Status: 200}:
			w.sent.Add(1)
		}
	}
}

type fakeDaemon struct{ w *fakeWorld }

func (d fakeDaemon) Stop() error {
	d.w.record(nil, "stop")
	if d.w.daemonDied {
		return errors.New("graywatchd had already exited")
	}
	return nil
}

func (w *fakeWorld) StartDaemon(ctx context.Context, eventsPath string, dryRun bool) (Daemon, error) {
	if err := w.record(ctx, fmt.Sprintf("start dry=%v %s", dryRun, eventsPath)); err != nil {
		return nil, err
	}
	return fakeDaemon{w}, nil
}

func (w *fakeWorld) log() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

// actions drops the sleeps and event reads, leaving the steps that change things.
func (w *fakeWorld) actions() []string {
	var out []string
	for _, c := range w.log() {
		if !strings.HasPrefix(c, "sleep ") && !strings.HasPrefix(c, "read-events ") {
			out = append(out, c)
		}
	}
	return out
}

type harness struct {
	w     *fakeWorld
	o     *Orchestrator
	saves *[]int
}

func newHarness() harness {
	w := newFakeWorld()
	saves := &[]int{}
	o := &Orchestrator{
		World: w, Now: w.Now, Sleep: w.Sleep, OutDir: "out", Seed: 42,
		Warmup: 60 * time.Second, PollEvery: time.Hour, Logf: func(string, ...any) {},
		Save: func(rs []TrialResult) error {
			w.record(nil, "save")
			*saves = append(*saves, len(rs))
			return nil
		},
	}
	return harness{w, o, saves}
}

func oneFault() Scenario {
	return Scenario{
		Name: "pool-leak", Fault: "pool-leak", ExpectedCategory: "app", Target: TargetRandom,
		Baseline: 30 * time.Second, FaultDur: 90 * time.Second, Cooldown: 30 * time.Second,
	}
}

var shallowDetect = Config{Health: "shallow", Mode: "detect"}

func TestParseConfigs(t *testing.T) {
	got, err := ParseConfigs("shallow:detect, shallow:remediate,deep:detect")
	if err != nil {
		t.Fatal(err)
	}
	want := []Config{{"shallow", "detect"}, {"shallow", "remediate"}, {"deep", "detect"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"", "shallow", "shallow:watch", "tall:detect", "shallow:detect:x"} {
		if _, err := ParseConfigs(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestRunFollowsThePlanInOrder(t *testing.T) {
	h := newHarness()
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d results", len(rs))
	}
	target := rs[0].Target
	want := []string{
		"revert-all",
		"up shallow",
		"ready",
		"mapping",
		"start dry=true out/events-shallow-detect.jsonl",
		"apply pool-leak " + target,
		"revert pool-leak " + target,
		"stop",
		"set-ready",
		"ready",
		"save",
		"revert-all", // the closing cleanup always runs, and finds nothing to do
		"set-ready",
		"save",
	}
	if got := h.w.actions(); !slices.Equal(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

func TestRunWaitsTheScenarioTimings(t *testing.T) {
	h := newHarness()
	if _, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1); err != nil {
		t.Fatal(err)
	}
	var sleeps []string
	for _, c := range h.w.log() {
		if d, ok := strings.CutPrefix(c, "sleep "); ok {
			sleeps = append(sleeps, d)
		}
	}
	want := []string{"1m0s", "30s", "1m30s", "30s"} // warmup, baseline, fault, cooldown
	if !slices.Equal(sleeps, want) {
		t.Fatalf("sleeps = %q, want %q", sleeps, want)
	}
}

func TestRunStartsADaemonForEachTrial(t *testing.T) {
	h := newHarness()
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d results", len(rs))
	}
	starts, stops := 0, 0
	for _, c := range h.w.log() {
		if strings.HasPrefix(c, "start ") {
			starts++
		}
		if c == "stop" {
			stops++
		}
	}
	if starts != 3 || stops != 3 {
		t.Fatalf("starts %d, stops %d, want 3 and 3", starts, stops)
	}
	for i, r := range rs {
		if r.Trial != i+1 {
			t.Errorf("result %d has Trial %d", i, r.Trial)
		}
		if r.Mode != "detect" || r.Health != "shallow" {
			t.Errorf("result %d: %+v", i, r)
		}
	}
}

func TestRunRemediateIsNotADryRun(t *testing.T) {
	h := newHarness()
	cfg := Config{Health: "deep", Mode: "remediate"}
	if _, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{cfg}, 1); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.w.log(), "up deep") {
		t.Errorf("no deep fleet: %q", h.w.log())
	}
	if !slices.Contains(h.w.log(), "start dry=false out/events-deep-remediate.jsonl") {
		t.Errorf("no real daemon: %q", h.w.log())
	}
}

func TestRunRecreatesTheFleetForEachConfig(t *testing.T) {
	h := newHarness()
	cfgs := []Config{shallowDetect, {Health: "deep", Mode: "detect"}}
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, cfgs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Health != "shallow" || rs[1].Health != "deep" {
		t.Fatalf("results = %+v", rs)
	}
	ups := 0
	for _, c := range h.w.log() {
		if strings.HasPrefix(c, "up ") {
			ups++
		}
	}
	if ups != 2 {
		t.Fatalf("%d fleet starts, want 2", ups)
	}
}

// The configs differ in one thing only, so they must see the same targets.
func TestRunPicksTheSameTargetsInEachConfig(t *testing.T) {
	h := newHarness()
	h.w.hosts = nil
	for i := 1; i <= 20; i++ {
		h.w.hosts = append(h.w.hosts, fmt.Sprintf("app-%02d", i))
	}
	cfgs := []Config{shallowDetect, {Health: "shallow", Mode: "remediate"}}
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, cfgs, 5)
	if err != nil {
		t.Fatal(err)
	}
	var first, second []string
	for _, r := range rs {
		if r.Mode == "detect" {
			first = append(first, r.Target)
		} else {
			second = append(second, r.Target)
		}
	}
	if !slices.Equal(first, second) {
		t.Fatalf("targets differ: %q vs %q", first, second)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(first)))) < 2 {
		t.Fatalf("targets are not random: %q", first)
	}
}

func TestRunNoneScenarioRunsOnceWithoutFault(t *testing.T) {
	h := newHarness()
	none := Scenario{Name: "none", Target: TargetNone, Baseline: 30 * time.Second, FaultDur: 600 * time.Second, Cooldown: 30 * time.Second}
	rs, err := h.o.Run(context.Background(), []Scenario{none}, []Config{shallowDetect}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Target != "" {
		t.Fatalf("results = %+v", rs)
	}
	for _, c := range h.w.log() {
		if strings.HasPrefix(c, "apply ") || strings.HasPrefix(c, "revert ") {
			t.Fatalf("unexpected %q", c)
		}
	}
}

func TestRunDepScenarioTargetsDep(t *testing.T) {
	h := newHarness()
	dep := oneFault()
	dep.Name, dep.Fault, dep.ExpectedCategory, dep.Target = "dep-slow", "dep-slow", "fleet", TargetDep
	rs, err := h.o.Run(context.Background(), []Scenario{dep}, []Config{shallowDetect}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Target != "dep" || !slices.Contains(h.w.log(), "apply dep-slow dep") {
		t.Fatalf("results %+v, calls %q", rs, h.w.log())
	}
}

func TestRunScoresTheDaemonsEvents(t *testing.T) {
	h := newHarness()
	h.w.hosts = []string{"app-07"}
	// Warmup 60 s and baseline 30 s pass on the fake clock before the fault.
	h.w.evs = []events.Event{{TS: t0.Add(96 * time.Second), Type: events.Suspect, Host: "app-07", Category: "app"}}
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !rs[0].GWDetected || rs[0].GWTTD != 6*time.Second || !rs[0].CategoryOK {
		t.Fatalf("result = %+v", rs[0])
	}
}

func TestRunSavesAfterEachTrial(t *testing.T) {
	h := newHarness()
	if _, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 3); err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 2, 3, 3}; !slices.Equal(*h.saves, want) {
		t.Fatalf("saved %v rows, want %v", *h.saves, want)
	}
}

// indexAfter finds the first call with this name after position from.
func indexAfter(calls []string, from int, name string) int {
	for i := from; i < len(calls); i++ {
		if calls[i] == name {
			return i
		}
	}
	return -1
}

func TestInterruptMidFaultCleansUpInOrder(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.cancel = cancel
	sc := oneFault()
	h.w.cancelWait = sc.FaultDur

	// One finished trial first, so the interrupt has a row to keep: the second
	// fault scenario's duration is what cancels.
	first := sc
	first.FaultDur = 80 * time.Second
	rs, err := h.o.Run(ctx, []Scenario{first, sc}, []Config{shallowDetect}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d finished trials, want 1", len(rs))
	}

	calls := h.w.actions()
	secondApply := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "apply ") {
			secondApply = i
		}
	}
	revertAll := indexAfter(calls, secondApply, "revert-all")
	setReady := indexAfter(calls, secondApply, "set-ready")
	stop := indexAfter(calls, secondApply, "stop")
	save := indexAfter(calls, secondApply, "save")
	if !(secondApply < revertAll && revertAll < setReady && setReady < stop && stop < save) {
		t.Fatalf("cleanup out of order: %q", calls[secondApply:])
	}
	if want := 1; (*h.saves)[len(*h.saves)-1] != want {
		t.Fatalf("final save had %d rows, want %d", (*h.saves)[len(*h.saves)-1], want)
	}
	for _, c := range calls[secondApply+1:] {
		if strings.HasPrefix(c, "revert ") {
			t.Fatalf("%q: an interrupt reverts through revert-all, which survives a crash", c)
		}
	}
	// The run's context is cancelled, and the cleanup must still work.
	for _, dead := range h.w.ctxDead {
		if dead == "revert-all" || dead == "set-ready" {
			t.Fatalf("%s ran on a cancelled context", dead)
		}
	}
}

func TestFailedApplyStopsTheRunAndCleansUp(t *testing.T) {
	h := newHarness()
	h.w.failOn["apply"] = errors.New("nsenter: permission denied")
	_, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 3)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v", err)
	}
	calls := h.w.actions()
	applies := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "apply ") {
			applies++
		}
	}
	if applies != 1 {
		t.Fatalf("%d applies, want the run to stop after the first", applies)
	}
	if !slices.Contains(calls, "stop") || calls[len(calls)-1] != "save" {
		t.Fatalf("no cleanup: %q", calls)
	}
}

func TestWrongHAProxyMappingStopsTheRun(t *testing.T) {
	h := newHarness()
	h.w.failOn["mapping"] = errors.New("18 servers point at the wrong container")
	_, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1)
	if err == nil || !strings.Contains(err.Error(), "wrong container") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range h.w.log() {
		if strings.HasPrefix(c, "start ") || strings.HasPrefix(c, "apply ") {
			t.Fatalf("%q ran on a mis-mapped fleet", c)
		}
	}
}

// A daemon that died mid-trial would otherwise score as a clean miss.
func TestDaemonDyingMidTrialFailsTheRunWithoutARow(t *testing.T) {
	h := newHarness()
	h.w.daemonDied = true
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1)
	if err == nil || !strings.Contains(err.Error(), "already exited") {
		t.Fatalf("err = %v", err)
	}
	if len(rs) != 0 {
		t.Fatalf("got %d results from a dead daemon", len(rs))
	}
}

func TestNoTrafficFailsTheRunWithoutARow(t *testing.T) {
	h := newHarness()
	h.w.noLoad = true
	rs, err := h.o.Run(context.Background(), []Scenario{oneFault()}, []Config{shallowDetect}, 1)
	if err == nil || !strings.Contains(err.Error(), "no requests") {
		t.Fatalf("err = %v", err)
	}
	if len(rs) != 0 {
		t.Fatalf("got %d results without traffic", len(rs))
	}
}

func TestPollRecordsDownServers(t *testing.T) {
	r := &trialRecorder{}
	r.observe(t0, []haproxy.ServerStat{
		{Name: "app-01", Status: "UP"},
		{Name: "app-02", Status: "DOWN"},
		{Name: "app-03", Status: "MAINT"},
		{Name: "app-04", Status: "DOWN 1/2"},
	})
	_, hc := r.take()
	want := []HCObs{{At: t0, Server: "app-02", Down: true}, {At: t0, Server: "app-04", Down: true}}
	if !slices.Equal(hc, want) {
		t.Fatalf("hc = %+v", hc)
	}
	if _, hc := r.take(); len(hc) != 0 {
		t.Fatalf("take must clear: %+v", hc)
	}
}

func TestAllUpMatchesByPrefix(t *testing.T) {
	stats := func(status ...string) []haproxy.ServerStat {
		var out []haproxy.ServerStat
		for i, s := range status {
			out = append(out, haproxy.ServerStat{Name: fmt.Sprintf("app-%02d", i), Status: s})
		}
		return out
	}
	if !AllUp(stats("UP", "UP 1/3"), 2) {
		t.Error(`"UP 1/3" is up: HAProxy is counting rises`)
	}
	for _, bad := range [][]string{{"UP"}, {"UP", "DOWN"}, {"UP", "MAINT"}, {"UP", "NOLB"}, {"UP", "DOWN 1/2"}} {
		if AllUp(stats(bad...), 2) {
			t.Errorf("%q counted as all up", bad)
		}
	}
}

func TestEstimateDuration(t *testing.T) {
	none := Scenario{Target: TargetNone, Baseline: 30 * time.Second, FaultDur: 600 * time.Second, Cooldown: 30 * time.Second}
	got := EstimateDuration([]Scenario{oneFault(), none}, 3, 3, time.Minute)
	// 3 configs x (60 s warmup + 3 x 150 s + 1 x 660 s)
	if want := 3 * (60 + 3*150 + 660) * time.Second; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCheckServerMapping(t *testing.T) {
	hosts := []fleet.Host{{Name: "app-01", IP: "172.18.0.2"}, {Name: "app-02", IP: "172.18.0.3"}}
	ok := map[string]string{"app-01": "172.18.0.2", "app-02": "172.18.0.3"}
	if err := CheckServerMapping(ok, hosts); err != nil {
		t.Fatal(err)
	}
	swapped := map[string]string{"app-01": "172.18.0.3", "app-02": "172.18.0.2"}
	err := CheckServerMapping(swapped, hosts)
	if err == nil || !strings.Contains(err.Error(), "app-01") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckServerMapping(map[string]string{"app-01": "172.18.0.2"}, hosts); err == nil {
		t.Fatal("a missing server must fail")
	}
	extra := map[string]string{"app-01": "172.18.0.2", "app-02": "172.18.0.3", "app-03": "172.18.0.4"}
	if err := CheckServerMapping(extra, hosts); err == nil {
		t.Fatal("a server with no container must fail")
	}
}

// A drain in flight when the interrupt lands can finish after the first set-ready,
// which would leave a server out after a "clean" stop.
func TestInterruptSetsServersReadyAgainAfterStoppingTheDaemon(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.cancel = cancel
	sc := oneFault()
	h.w.cancelWait = sc.FaultDur
	if _, err := h.o.Run(ctx, []Scenario{sc}, []Config{shallowDetect}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	calls := h.w.actions()
	stop := indexAfter(calls, 0, "stop")
	if stop < 0 || indexAfter(calls, stop, "set-ready") < 0 {
		t.Fatalf("no set-ready after stop: %q", calls)
	}
}

func TestCheckOutputProtectsEarlierResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trials.csv")
	if err := CheckOutput(path, false); err != nil {
		t.Fatalf("no file yet: %v", err)
	}
	if err := SaveTrialsCSV(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckOutput(path, false); err != nil {
		t.Fatalf("header only: %v", err)
	}
	if err := SaveTrialsCSV(path, []TrialResult{{Scenario: "x"}}); err != nil {
		t.Fatal(err)
	}
	err := CheckOutput(path, false)
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("err = %v, want it to refuse and mention -force", err)
	}
	if err := CheckOutput(path, true); err != nil {
		t.Fatalf("with force: %v", err)
	}
}
