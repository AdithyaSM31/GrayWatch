package bench

import (
	"strings"
	"testing"
	"time"
)

func TestRenderEBPFComparesTheSameFaults(t *testing.T) {
	base := []TrialResult{
		trial("cpu-throttle", "shallow", "detect", true, 3*time.Second),
		trial("pool-leak", "shallow", "detect", true, 3*time.Second),
		trial("none", "shallow", "detect", false, 0),
	}
	with := []TrialResult{
		trial("cpu-throttle", "shallow", "detect", true, 2*time.Second),
		{Scenario: "none", Health: "shallow", Mode: "detect", FalsePosHosts: 1},
	}
	out, err := RenderEBPF(base, with, 20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Fault | Graywatch caught | Median TTD | Graywatch caught (eBPF) | Median TTD (eBPF) | p99 detect-only | p99 detect-only (eBPF) |",
		"| cpu-throttle | 1/1 | 3.0 s | 1/1 | 2.0 s | 0 ms | 0 ms |",
		"0.05 per host-hour across 20.00 host-hours",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pool-leak") {
		t.Errorf("pool-leak has no eBPF run:\n%s", out)
	}
}

// Comparing against rows from another config would credit eBPF for the config.
func TestRenderEBPFUsesOnlyMatchingConfigs(t *testing.T) {
	base := []TrialResult{
		trial("cpu-throttle", "shallow", "detect", true, 3*time.Second),
		trial("cpu-throttle", "deep", "detect", true, 9*time.Second),
		trial("cpu-throttle", "shallow", "remediate", true, 9*time.Second),
	}
	with := []TrialResult{trial("cpu-throttle", "shallow", "detect", true, 2*time.Second)}
	out, err := RenderEBPF(base, with, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "| cpu-throttle | 1/1 | 3.0 s | 1/1 | 2.0 s |") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestReportAddsEBPFSection(t *testing.T) {
	none := TrialResult{Scenario: "none", Mode: "detect", Health: "shallow"}
	out, err := Report([]TrialResult{none}, 20, 11*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "eBPF") {
		t.Fatalf("no eBPF results were given:\n%s", out)
	}
}
