package detect

import (
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/collect"
)

// runqState holds one host at a p99 for long enough to debounce, in a fleet with the
// given median, and reports what became of it.
func runqState(t *testing.T, median, host float64) HostState {
	t.Helper()
	w := newWorld(t, 20)
	w.normal[collect.SigRunqP99] = median
	var obs Observation
	for range 5 {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-01": {collect.SigRunqP99: host}})
	}
	return verdict(t, obs, "app-01").State
}

// The p99 comes from a log2 histogram, so it can only double. One bucket above the
// median is noise at any median, however large that step is in microseconds.
func TestRunqOneBucketAboveTheMedianIsNotAFault(t *testing.T) {
	for _, median := range []float64{1024, 2048, 4096, 8192, 32768} {
		if got := runqState(t, median, median*2); got != Healthy {
			t.Errorf("median %.0f, host %.0f: %s", median, median*2, got)
		}
	}
}

func TestRunqTwoBucketsAboveTheMedianIsAFault(t *testing.T) {
	for _, median := range []float64{2048, 4096, 8192, 32768} {
		if got := runqState(t, median, median*4); got != Suspect {
			t.Errorf("median %.0f, host %.0f: %s", median, median*4, got)
		}
	}
}

// At a low median the ratio alone would flag waits nobody would notice.
func TestRunqStillNeedsAnAbsoluteDifference(t *testing.T) {
	if got := runqState(t, 256, 1024); got != Healthy {
		t.Errorf("median 256, host 1024: %s", got)
	}
}

// Other signals have no ratio, so this must not change them.
func TestSignalsWithNoRatioAreJudgedAsBefore(t *testing.T) {
	w := newWorld(t, 20)
	w.normal[collect.SigLatP90] = 500
	var obs Observation
	for range 5 {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-01": {collect.SigLatP90: 600}})
	}
	if got := verdict(t, obs, "app-01").State; got != Suspect {
		t.Errorf("latency 100 ms above the median: %s", got)
	}
}

// Run queue waits follow from most kinds of trouble, and its z-score dwarfs the others
// because its floor is so low. So it names the cause only when nothing else does.
func TestRunqDoesNotOutrankAnotherCause(t *testing.T) {
	w := newWorld(t, 20)
	w.normal[collect.SigRunqP99] = 2048
	w.normal[collect.SigMemPSI] = 0.01
	var obs Observation
	for range 5 {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-01": {collect.SigRunqP99: 131072, collect.SigMemPSI: 0.6}})
	}
	if got := verdict(t, obs, "app-01").Category; got != "memory" {
		t.Errorf("category = %q, want memory", got)
	}
}

func TestRunqAloneReadsAsCPU(t *testing.T) {
	w := newWorld(t, 20)
	w.normal[collect.SigRunqP99] = 2048
	var obs Observation
	for range 5 {
		obs = w.tick(map[string]map[collect.Signal]float64{"app-01": {collect.SigRunqP99: 131072}})
	}
	if got := verdict(t, obs, "app-01").Category; got != "cpu" {
		t.Errorf("category = %q, want cpu", got)
	}
}
