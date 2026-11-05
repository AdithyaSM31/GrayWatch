package detect

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/collect"
)

var startTime = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// world feeds a detector one tick at a time. Each host starts every tick with
// the given normal values, and tests override what they care about.
type world struct {
	t       *testing.T
	d       *Detector
	hosts   []string
	normal  map[collect.Signal]float64
	tickNum int
	drained map[string]bool
}

func newWorld(t *testing.T, n int) *world {
	t.Helper()
	w := &world{
		t:      t,
		d:      New(DefaultConfig()),
		normal: map[collect.Signal]float64{collect.SigIOPSI: 0.01},
	}
	for i := 1; i <= n; i++ {
		w.hosts = append(w.hosts, fmt.Sprintf("app-%02d", i))
	}
	return w
}

// tick builds samples and observes them. Overrides are keyed by host name.
func (w *world) tick(overrides map[string]map[collect.Signal]float64) Observation {
	w.t.Helper()
	w.tickNum++
	at := startTime.Add(time.Duration(w.tickNum) * time.Second)
	samples := make([]collect.Sample, 0, len(w.hosts))
	for _, h := range w.hosts {
		values := map[collect.Signal]float64{}
		for sig, v := range w.normal {
			values[sig] = v
		}
		for sig, v := range overrides[h] {
			values[sig] = v
		}
		samples = append(samples, collect.Sample{Host: h, At: at, Values: values})
	}
	return w.d.Observe(samples, w.drained)
}

func verdict(t *testing.T, obs Observation, host string) Verdict {
	t.Helper()
	for _, v := range obs.Verdicts {
		if v.Host == host {
			return v
		}
	}
	t.Fatalf("no verdict for %s", host)
	return Verdict{}
}

func suspects(obs Observation) []string {
	var names []string
	for _, v := range obs.Verdicts {
		if v.State == Suspect {
			names = append(names, v.Host)
		}
	}
	return names
}

func sig(s collect.Signal, v float64) map[collect.Signal]float64 {
	return map[collect.Signal]float64{s: v}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Z != 3.5 || c.Window != 5 || c.Need != 3 || c.MinHosts != 5 || c.FleetLatencyMs != 300 || c.FleetErrRate != 0.05 {
		t.Errorf("config = %+v", c)
	}
	for _, tc := range []struct {
		s                collect.Signal
		minMAD, minDelta float64
		category         string
	}{
		{collect.SigLatP90, 5, 50, "app"},
		{collect.SigErrRate, 0.005, 0.02, "app"},
		{collect.SigCPUPSI, 0.01, 0.10, "cpu"},
		{collect.SigThrottle, 0.01, 0.10, "cpu"},
		{collect.SigIOPSI, 0.01, 0.10, "disk"},
		{collect.SigMemPSI, 0.01, 0.05, "memory"},
		{collect.SigRetrans, 0.002, 0.01, "network"},
	} {
		if c.MinMAD[tc.s] != tc.minMAD || c.MinDelta[tc.s] != tc.minDelta || c.Category[tc.s] != tc.category {
			t.Errorf("%s: MinMAD/MinDelta/Category = %v/%v/%q", tc.s, c.MinMAD[tc.s], c.MinDelta[tc.s], c.Category[tc.s])
		}
	}
}

func TestSingleSpikeStaysHealthy(t *testing.T) {
	w := newWorld(t, 20)
	spike := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	for i := 0; i < 5; i++ {
		var obs Observation
		if i == 2 {
			obs = w.tick(spike)
		} else {
			obs = w.tick(nil)
		}
		if got := verdict(t, obs, "app-03").State; got != Healthy {
			t.Fatalf("tick %d: state = %v, want healthy", i, got)
		}
	}
}

func TestSustainedOutlierBecomesSuspect(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	for i := 1; i <= 2; i++ {
		if got := verdict(t, w.tick(bad), "app-03").State; got != Healthy {
			t.Fatalf("tick %d: state = %v, want healthy", i, got)
		}
	}
	obs := w.tick(bad)
	v := verdict(t, obs, "app-03")
	if v.State != Suspect {
		t.Fatalf("tick 3: state = %v, want suspect", v.State)
	}
	if !slices.Contains(v.Signals, collect.SigIOPSI) {
		t.Errorf("signals = %v, want io_psi", v.Signals)
	}
	if v.Category != "disk" {
		t.Errorf("category = %q, want disk", v.Category)
	}
	if !v.Since.Equal(obs.At) {
		t.Errorf("since = %v, want this tick %v", v.Since, obs.At)
	}
	want := 0.6745 * (0.5 - 0.01) / 0.01
	if math.Abs(v.Score-want) > 1e-6 {
		t.Errorf("score = %v, want %v", v.Score, want)
	}
	if got := suspects(obs); len(got) != 1 {
		t.Errorf("suspects = %v, want only app-03", got)
	}
}

func TestNonConsecutiveHitsCount(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	w.tick(bad)
	w.tick(nil)
	w.tick(bad)
	w.tick(nil)
	if got := verdict(t, w.tick(bad), "app-03").State; got != Suspect {
		t.Fatalf("state = %v, want suspect after 3 hits in 5 ticks", got)
	}
}

func TestSparseHitsNeverAccumulate(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	for i := 0; i < 30; i++ {
		var obs Observation
		if i%3 == 0 { // never more than 2 in any 5
			obs = w.tick(bad)
		} else {
			obs = w.tick(nil)
		}
		if got := verdict(t, obs, "app-03").State; got != Healthy {
			t.Fatalf("tick %d: state = %v, want healthy", i, got)
		}
	}
}

func TestRecoversAfterQuietWindow(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	for i := 0; i < 3; i++ {
		w.tick(bad)
	}
	for i := 1; i <= 4; i++ {
		if got := verdict(t, w.tick(nil), "app-03").State; got != Suspect {
			t.Fatalf("clean tick %d: state = %v, want still suspect", i, got)
		}
	}
	obs := w.tick(nil)
	v := verdict(t, obs, "app-03")
	if v.State != Healthy {
		t.Fatalf("clean tick 5: state = %v, want healthy", v.State)
	}
	if !v.Since.Equal(obs.At) {
		t.Errorf("since = %v, want this tick %v", v.Since, obs.At)
	}
	if len(v.Signals) != 0 || v.Category != "" || v.Score != 0 {
		t.Errorf("healthy verdict carries %+v", v)
	}
}

// While a host stays suspect through clean ticks, the planner still needs to
// know why it is suspect and how bad it was.
func TestSuspectKeepsReasonThroughCleanTicks(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	for i := 0; i < 3; i++ {
		w.tick(bad)
	}
	v := verdict(t, w.tick(nil), "app-03")
	if v.Category != "disk" || v.Score <= 0 || !slices.Contains(v.Signals, collect.SigIOPSI) {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestScoreComesFromLatestHitTick(t *testing.T) {
	w := newWorld(t, 20)
	for i := 0; i < 3; i++ {
		w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.9)})
	}
	v := verdict(t, w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}), "app-03")
	want := 0.6745 * (0.5 - 0.01) / 0.01
	if math.Abs(v.Score-want) > 1e-6 {
		t.Fatalf("score = %v, want %v from the latest tick", v.Score, want)
	}
}

func TestScoreIsHighestFlaggedZ(t *testing.T) {
	w := newWorld(t, 20)
	w.normal[collect.SigCPUPSI] = 0.01
	var v Verdict
	for i := 0; i < 3; i++ {
		v = verdict(t, w.tick(map[string]map[collect.Signal]float64{
			"app-03": {collect.SigIOPSI: 0.5, collect.SigCPUPSI: 0.9},
		}), "app-03")
	}
	want := 0.6745 * (0.9 - 0.01) / 0.01
	if math.Abs(v.Score-want) > 1e-6 {
		t.Fatalf("score = %v, want %v", v.Score, want)
	}
}

func TestSinceStaysAtTheTransition(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-03": sig(collect.SigIOPSI, 0.5)}
	w.tick(bad)
	w.tick(bad)
	first := w.tick(bad)
	later := w.tick(bad)
	if got := verdict(t, later, "app-03").Since; !got.Equal(first.At) {
		t.Fatalf("since = %v, want the transition at %v", got, first.At)
	}
}

func TestTooFewHostsSkipsSignal(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{}
	reporting := map[string]map[collect.Signal]float64{
		"app-01": sig(collect.SigCPUPSI, 0.01),
		"app-02": sig(collect.SigCPUPSI, 0.01),
		"app-03": sig(collect.SigCPUPSI, 0.01),
		"app-04": sig(collect.SigCPUPSI, 0.9),
	}
	for i := 0; i < 10; i++ {
		if got := suspects(w.tick(reporting)); len(got) != 0 {
			t.Fatalf("tick %d: suspects = %v, want none with only 4 hosts reporting", i, got)
		}
	}
}

func TestExactlyMinHostsIsEnough(t *testing.T) {
	w := newWorld(t, 5)
	w.normal = map[collect.Signal]float64{collect.SigCPUPSI: 0.01}
	bad := map[string]map[collect.Signal]float64{"app-05": sig(collect.SigCPUPSI, 0.9)}
	var obs Observation
	for i := 0; i < 3; i++ {
		obs = w.tick(bad)
	}
	if got := suspects(obs); !slices.Equal(got, []string{"app-05"}) {
		t.Fatalf("suspects = %v, want app-05", got)
	}
}

func TestSmallAbsoluteDeltaIgnored(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 10}
	// 12 ms fails both tests. 59 ms has z of about 6.6, so only MinDelta stops it.
	for _, lat := range []float64{12, 59} {
		for i := 0; i < 6; i++ {
			obs := w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigLatP90, lat)})
			if got := suspects(obs); len(got) != 0 {
				t.Fatalf("lat %v: suspects = %v, want none", lat, got)
			}
		}
	}
	for i := 0; i < 3; i++ {
		w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigLatP90, 61)})
	}
	obs := w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigLatP90, 61)})
	if got := suspects(obs); !slices.Equal(got, []string{"app-03"}) {
		t.Fatalf("61 ms: suspects = %v, want app-03 just past MinDelta", got)
	}
}

// A big absolute gap is not an outlier when the whole fleet is spread out.
func TestLargeDeltaInSpreadFleetIgnored(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = nil
	spread := map[string]map[collect.Signal]float64{}
	for i, h := range w.hosts {
		spread[h] = sig(collect.SigLatP90, float64(100+i*40))
	}
	for i := 0; i < 6; i++ {
		if got := suspects(w.tick(spread)); len(got) != 0 {
			t.Fatalf("suspects = %v, want none", got)
		}
	}
}

func TestBetterThanFleetIsNotAnOutlier(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 500}
	for i := 0; i < 6; i++ {
		obs := w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigLatP90, 1)})
		if got := suspects(obs); len(got) != 0 {
			t.Fatalf("suspects = %v, want none", got)
		}
	}
}

// Review Focus 2: 19 hosts at exactly 0 errors and one at 3%.
func TestUniformFleetStillFlagsOutlier(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigErrRate: 0}
	var obs Observation
	for i := 0; i < 3; i++ {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigErrRate, 0.03)})
	}
	v := verdict(t, obs, "app-03")
	if v.State != Suspect || math.IsInf(v.Score, 0) || math.IsNaN(v.Score) {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestSymptomOnlyIsApp(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigErrRate: 0}
	var v Verdict
	for i := 0; i < 3; i++ {
		v = verdict(t, w.tick(map[string]map[collect.Signal]float64{"app-03": sig(collect.SigErrRate, 0.5)}), "app-03")
	}
	if v.State != Suspect || v.Category != "app" {
		t.Fatalf("verdict = %+v, want suspect in category app", v)
	}
}

// Latency and errors are symptoms. A kernel signal names the cause, so it wins
// even when the symptom's z-score is far higher.
func TestCategoryPrefersKernelSignal(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigErrRate: 0, collect.SigCPUPSI: 0.01}
	var v Verdict
	for i := 0; i < 3; i++ {
		v = verdict(t, w.tick(map[string]map[collect.Signal]float64{
			"app-03": {collect.SigErrRate: 1, collect.SigCPUPSI: 0.5},
		}), "app-03")
	}
	if v.Category != "cpu" {
		t.Fatalf("category = %q, want cpu", v.Category)
	}
	if !slices.Contains(v.Signals, collect.SigErrRate) || !slices.Contains(v.Signals, collect.SigCPUPSI) {
		t.Fatalf("signals = %v, want both", v.Signals)
	}
}

func TestCategoryPicksHighestKernelZ(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigIOPSI: 0.01, collect.SigCPUPSI: 0.01}
	var v Verdict
	for i := 0; i < 3; i++ {
		v = verdict(t, w.tick(map[string]map[collect.Signal]float64{
			"app-03": {collect.SigIOPSI: 0.3, collect.SigCPUPSI: 0.9},
		}), "app-03")
	}
	if v.Category != "cpu" {
		t.Fatalf("category = %q, want cpu from the higher z", v.Category)
	}
}

// Review Focus 3: a drained host has no client signals, which is neither healthy
// evidence nor an outlier.
func TestMissingClientSignalsNotEvidence(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 10, collect.SigErrRate: 0, collect.SigIOPSI: 0.01}
	w.drained = map[string]bool{"app-05": true}

	samples := func(at time.Time) []collect.Sample {
		var out []collect.Sample
		for _, h := range w.hosts {
			values := map[collect.Signal]float64{collect.SigLatP90: 10, collect.SigErrRate: 0, collect.SigIOPSI: 0.01}
			if h == "app-05" {
				values = map[collect.Signal]float64{collect.SigIOPSI: 0.01}
			}
			out = append(out, collect.Sample{Host: h, At: at, Values: values})
		}
		return out
	}
	for i := 0; i < 6; i++ {
		obs := w.d.Observe(samples(startTime.Add(time.Duration(i)*time.Second)), w.drained)
		if got := verdict(t, obs, "app-05").State; got != Healthy {
			t.Fatalf("tick %d: drained host state = %v, want healthy", i, got)
		}
		if got := suspects(obs); len(got) != 0 {
			t.Fatalf("tick %d: suspects = %v", i, got)
		}
	}
}

// The drained hosts' values would pull the median up to 0.255 and hide app-01.
func TestDrainedHostsExcludedFromPeers(t *testing.T) {
	w := newWorld(t, 8)
	w.drained = map[string]bool{"app-06": true, "app-07": true, "app-08": true}
	bad := map[string]map[collect.Signal]float64{
		"app-01": sig(collect.SigIOPSI, 0.5),
		"app-06": sig(collect.SigIOPSI, 0.5),
		"app-07": sig(collect.SigIOPSI, 0.5),
		"app-08": sig(collect.SigIOPSI, 0.5),
	}
	var obs Observation
	for i := 0; i < 3; i++ {
		obs = w.tick(bad)
	}
	if got := verdict(t, obs, "app-01").State; got != Suspect {
		t.Fatalf("app-01 state = %v, want suspect", got)
	}
}

// A drained host still sick at the kernel level must not look ready to restore.
func TestDrainedHostStillJudgedOnKernelSignals(t *testing.T) {
	w := newWorld(t, 20)
	w.drained = map[string]bool{"app-05": true}
	var obs Observation
	for i := 0; i < 3; i++ {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-05": sig(collect.SigIOPSI, 0.5)})
	}
	if got := verdict(t, obs, "app-05").State; got != Suspect {
		t.Fatalf("state = %v, want suspect", got)
	}
}

func TestFleetWideShiftRaisesFleetAlertNotSuspects(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 400}
	var obs Observation
	for i := 0; i < 6; i++ {
		obs = w.tick(nil)
		if got := suspects(obs); len(got) != 0 {
			t.Fatalf("suspects = %v, want none", got)
		}
	}
	if obs.Fleet == nil {
		t.Fatal("no fleet alert")
	}
	if obs.Fleet.MedianLatMs != 400 || obs.Fleet.Reason == "" {
		t.Fatalf("fleet = %+v", obs.Fleet)
	}
}

func TestFleetAlertOnErrors(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigErrRate: 0.2}
	obs := w.tick(nil)
	if obs.Fleet == nil || obs.Fleet.MedianErr != 0.2 {
		t.Fatalf("fleet = %+v", obs.Fleet)
	}
}

func TestFleetAlertReasonNamesTheCause(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 400, collect.SigErrRate: 0}
	if got := w.tick(nil).Fleet; got == nil || !strings.Contains(got.Reason, "latency") {
		t.Fatalf("fleet = %+v, want a reason naming latency", got)
	}
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 10, collect.SigErrRate: 0.2}
	if got := w.tick(nil).Fleet; got == nil || !strings.Contains(got.Reason, "error") {
		t.Fatalf("fleet = %+v, want a reason naming errors", got)
	}
}

func TestNoFleetAlertAtOrBelowThresholds(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 300, collect.SigErrRate: 0.05}
	if got := w.tick(nil).Fleet; got != nil {
		t.Fatalf("fleet = %+v, want nil exactly at the thresholds", got)
	}
}

// One sick host is not a fleet problem, however sick.
func TestOneBadHostIsNoFleetAlert(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 10, collect.SigErrRate: 0}
	obs := w.tick(map[string]map[collect.Signal]float64{
		"app-03": {collect.SigLatP90: 5000, collect.SigErrRate: 1},
	})
	if obs.Fleet != nil {
		t.Fatalf("fleet = %+v, want nil", obs.Fleet)
	}
}

// A handful of hosts with data is not "the fleet". During startup, one slow host
// must not raise an alert that blocks every drain.
func TestFleetAlertNeedsMinHosts(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = nil
	slow := map[string]map[collect.Signal]float64{}
	for _, h := range w.hosts[:4] {
		slow[h] = sig(collect.SigLatP90, 5000)
	}
	if got := w.tick(slow).Fleet; got != nil {
		t.Fatalf("fleet = %+v, want nil with only 4 hosts reporting", got)
	}
}

func TestFleetAlertExcludesDrainedHosts(t *testing.T) {
	w := newWorld(t, 8)
	w.normal = map[collect.Signal]float64{collect.SigLatP90: 10}
	w.drained = map[string]bool{"app-06": true, "app-07": true, "app-08": true}
	bad := map[string]map[collect.Signal]float64{
		"app-05": sig(collect.SigLatP90, 5000),
		"app-06": sig(collect.SigLatP90, 5000),
		"app-07": sig(collect.SigLatP90, 5000),
		"app-08": sig(collect.SigLatP90, 5000),
	}
	if got := w.tick(bad).Fleet; got != nil {
		t.Fatalf("fleet = %+v, want nil: the drained hosts are not counted", got)
	}
}

func TestVerdictsSortedAndCoverEveryHost(t *testing.T) {
	d := New(DefaultConfig())
	obs := d.Observe([]collect.Sample{
		{Host: "app-03", At: startTime},
		{Host: "app-01", At: startTime},
		{Host: "app-02", At: startTime},
	}, nil)
	var names []string
	for _, v := range obs.Verdicts {
		names = append(names, v.Host)
	}
	if !slices.Equal(names, []string{"app-01", "app-02", "app-03"}) {
		t.Fatalf("verdicts = %v", names)
	}
	if !obs.At.Equal(startTime) {
		t.Fatalf("At = %v, want %v", obs.At, startTime)
	}
}

func TestSignalsListIsSorted(t *testing.T) {
	w := newWorld(t, 20)
	w.normal = map[collect.Signal]float64{collect.SigIOPSI: 0.01, collect.SigCPUPSI: 0.01, collect.SigErrRate: 0}
	var v Verdict
	for i := 0; i < 3; i++ {
		v = verdict(t, w.tick(map[string]map[collect.Signal]float64{
			"app-03": {collect.SigIOPSI: 0.5, collect.SigCPUPSI: 0.5, collect.SigErrRate: 0.5},
		}), "app-03")
	}
	if !slices.IsSorted(v.Signals) || len(v.Signals) != 3 {
		t.Fatalf("signals = %v, want 3 in sorted order", v.Signals)
	}
}

func TestEmptyObservation(t *testing.T) {
	obs := New(DefaultConfig()).Observe(nil, nil)
	if len(obs.Verdicts) != 0 || obs.Fleet != nil {
		t.Fatalf("obs = %+v", obs)
	}
}

// A host that left the fleet and came back has a new container, so its old
// history says nothing about it.
func TestRemovedHostIsForgotten(t *testing.T) {
	w := newWorld(t, 20)
	bad := map[string]map[collect.Signal]float64{"app-20": sig(collect.SigIOPSI, 0.5)}
	var obs Observation
	for i := 0; i < 3; i++ {
		obs = w.tick(bad)
	}
	if got := verdict(t, obs, "app-20").State; got != Suspect {
		t.Fatalf("state = %v, want suspect", got)
	}
	all := w.hosts
	w.hosts = all[:19]
	w.tick(nil)
	w.hosts = all
	if got := verdict(t, w.tick(nil), "app-20").State; got != Healthy {
		t.Fatalf("state = %v, want healthy for a returning host", got)
	}
}

func TestDetectorDoesNotReadConfigFromPackageState(t *testing.T) {
	c := DefaultConfig()
	c.Need = 1
	d := New(c)
	var samples []collect.Sample
	for i := 1; i <= 6; i++ {
		v := 0.01
		if i == 6 {
			v = 0.5
		}
		samples = append(samples, collect.Sample{
			Host: fmt.Sprintf("app-%02d", i), At: startTime,
			Values: map[collect.Signal]float64{collect.SigIOPSI: v},
		})
	}
	if got := suspects(d.Observe(samples, nil)); !slices.Equal(got, []string{"app-06"}) {
		t.Fatalf("suspects = %v, want app-06 with Need 1", got)
	}
}
