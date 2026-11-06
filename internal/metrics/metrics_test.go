package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/detect"
)

func record(m *Metrics, values map[collect.Signal]float64, v detect.Verdict) {
	m.Record(
		[]collect.Sample{{Host: v.Host, Values: values}},
		detect.Observation{Verdicts: []detect.Verdict{v}},
	)
}

func TestRecordSetsGauges(t *testing.T) {
	m := New(prometheus.NewRegistry())
	record(m, map[collect.Signal]float64{collect.SigIOPSI: 0.4}, detect.Verdict{
		Host: "app-07", State: detect.Suspect, Z: map[collect.Signal]float64{collect.SigIOPSI: 26.3},
	})
	if got := testutil.ToFloat64(m.signal.WithLabelValues("app-07", "io_psi")); got != 0.4 {
		t.Errorf("signal = %v", got)
	}
	if got := testutil.ToFloat64(m.z.WithLabelValues("app-07", "io_psi")); got != 26.3 {
		t.Errorf("z = %v", got)
	}
	if got := testutil.ToFloat64(m.suspect.WithLabelValues("app-07")); got != 1 {
		t.Errorf("suspect = %v", got)
	}
}

func TestHealthyHostReadsZero(t *testing.T) {
	m := New(prometheus.NewRegistry())
	record(m, nil, detect.Verdict{Host: "app-07", State: detect.Healthy})
	if got := testutil.ToFloat64(m.suspect.WithLabelValues("app-07")); got != 0 {
		t.Errorf("suspect = %v", got)
	}
}

// A drained host has no latency. Keeping the last value would draw a flat line
// of data that does not exist, which is the difference between missing and zero.
func TestMissingSignalIsRemoved(t *testing.T) {
	m := New(prometheus.NewRegistry())
	record(m, map[collect.Signal]float64{collect.SigLatP90: 10, collect.SigIOPSI: 0.1}, detect.Verdict{
		Host: "app-07", Z: map[collect.Signal]float64{collect.SigLatP90: 1, collect.SigIOPSI: 2},
	})
	if got := testutil.CollectAndCount(m.signal); got != 2 {
		t.Fatalf("signal series = %d, want 2", got)
	}
	record(m, map[collect.Signal]float64{collect.SigIOPSI: 0.1}, detect.Verdict{
		Host: "app-07", Z: map[collect.Signal]float64{collect.SigIOPSI: 2},
	})
	if got := testutil.CollectAndCount(m.signal); got != 1 {
		t.Errorf("signal series = %d, want the latency one gone", got)
	}
	if got := testutil.CollectAndCount(m.z); got != 1 {
		t.Errorf("z series = %d, want the latency one gone", got)
	}
}

func TestRemovedHostIsRemoved(t *testing.T) {
	m := New(prometheus.NewRegistry())
	record(m, map[collect.Signal]float64{collect.SigIOPSI: 0.1}, detect.Verdict{Host: "app-07"})
	m.Record(nil, detect.Observation{})
	if got := testutil.CollectAndCount(m.signal); got != 0 {
		t.Errorf("signal series = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(m.suspect); got != 0 {
		t.Errorf("suspect series = %d, want 0", got)
	}
}

func TestCountAction(t *testing.T) {
	m := New(prometheus.NewRegistry())
	m.CountAction("drain")
	m.CountAction("drain")
	m.CountAction("restore")
	if got := testutil.ToFloat64(m.actions.WithLabelValues("drain")); got != 2 {
		t.Errorf("drain = %v", got)
	}
}

func TestMetricNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	record(m, map[collect.Signal]float64{collect.SigIOPSI: 0.1}, detect.Verdict{
		Host: "app-07", Z: map[collect.Signal]float64{collect.SigIOPSI: 2},
	})
	m.CountAction("drain")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{"graywatch_signal", "graywatch_robust_z", "graywatch_host_suspect", "graywatch_actions_total"} {
		if !names[want] {
			t.Errorf("metric %s missing, have %v", want, names)
		}
	}
}
