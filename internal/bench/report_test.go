package bench

import (
	"bytes"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func trial(scenario, health, mode string, detected bool, ttd time.Duration) TrialResult {
	return TrialResult{Scenario: scenario, Health: health, Mode: mode, GWDetected: detected, GWTTD: ttd}
}

func findRow(t *testing.T, rows []Row, fault string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Fault == fault {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", fault, rows)
	return Row{}
}

func TestSummarizeRates(t *testing.T) {
	rows := Summarize([]TrialResult{
		trial("pool-leak", "shallow", "detect", true, 6*time.Second),
		trial("pool-leak", "shallow", "detect", false, 0),
		trial("pool-leak", "shallow", "detect", true, 4*time.Second),
	})
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	r := rows[0]
	if r.Graywatch != "2/3" {
		t.Errorf("Graywatch = %q, want 2/3", r.Graywatch)
	}
	if r.MedianTTD != 5*time.Second {
		t.Errorf("MedianTTD = %v, want the median of detected trials only", r.MedianTTD)
	}
}

func TestSummarizeMedianOfOddCount(t *testing.T) {
	r := Summarize([]TrialResult{
		trial("x", "shallow", "detect", true, 9*time.Second),
		trial("x", "shallow", "detect", true, 3*time.Second),
		trial("x", "shallow", "detect", true, 4*time.Second),
	})[0]
	if r.MedianTTD != 4*time.Second {
		t.Fatalf("MedianTTD = %v, want 4s", r.MedianTTD)
	}
}

// Graywatch drains first in a remediate run, and a server in MAINT cannot read
// DOWN. Those rows say nothing about what the health check would have done.
func TestSummarizeHealthCheckIgnoresRemediateRows(t *testing.T) {
	hc := func(health, mode string, caught bool) TrialResult {
		r := trial("mem", health, mode, true, time.Second)
		r.HCDetected = caught
		return r
	}
	r := Summarize([]TrialResult{
		hc("shallow", "detect", true),
		hc("shallow", "detect", false),
		hc("shallow", "remediate", false),
		hc("shallow", "remediate", false),
		hc("deep", "detect", true),
	})[0]
	if r.HCShallow != "1/2" {
		t.Errorf("HCShallow = %q, want 1/2", r.HCShallow)
	}
	if r.HCDeep != "1/1" {
		t.Errorf("HCDeep = %q, want 1/1", r.HCDeep)
	}
	if r.Graywatch != "5/5" {
		t.Errorf("Graywatch = %q, want every row counted", r.Graywatch)
	}
}

func TestSummarizeCategoryAndFalsePositives(t *testing.T) {
	a := trial("x", "shallow", "detect", true, time.Second)
	a.CategoryOK, a.FalsePosHosts = true, 2
	b := trial("x", "deep", "detect", true, time.Second)
	b.FalsePosHosts = 1
	r := Summarize([]TrialResult{a, b})[0]
	if r.Category != "1/2" || r.FalsePosHosts != 3 {
		t.Fatalf("row = %+v", r)
	}
}

// Deep rows do not count as detect-only: there, the health check takes the host out.
func TestSummarizeLatencyAndErrorsByConfig(t *testing.T) {
	row := func(health, mode string, p99 time.Duration, errRate float64) TrialResult {
		r := trial("x", health, mode, true, time.Second)
		r.P99, r.ErrRate = p99, errRate
		return r
	}
	r := Summarize([]TrialResult{
		row("shallow", "detect", 100*time.Millisecond, 0.04),
		row("shallow", "detect", 300*time.Millisecond, 0.06),
		row("shallow", "remediate", 20*time.Millisecond, 0.01),
		row("deep", "detect", 50*time.Millisecond, 0.02),
	})[0]
	if r.P99Detect != 200*time.Millisecond || r.P99Remediate != 20*time.Millisecond {
		t.Errorf("p99 detect %v, remediate %v", r.P99Detect, r.P99Remediate)
	}
	if r.ErrDetect != 0.05 || r.ErrRemediate != 0.01 || r.ErrDeep != 0.02 {
		t.Errorf("errors %v %v %v", r.ErrDetect, r.ErrRemediate, r.ErrDeep)
	}
}

func TestSummarizeLeavesOutTheBaselineAndSorts(t *testing.T) {
	rows := Summarize([]TrialResult{
		trial("pool-leak", "shallow", "detect", true, time.Second),
		trial("none", "shallow", "detect", false, 0),
		trial("cpu-throttle", "shallow", "detect", true, time.Second),
	})
	var names []string
	for _, r := range rows {
		names = append(names, r.Fault)
	}
	if !slices.Equal(names, []string{"cpu-throttle", "pool-leak"}) {
		t.Fatalf("rows = %q", names)
	}
}

func TestSummarizeCountsFleetAlerts(t *testing.T) {
	a := trial("x", "shallow", "detect", true, time.Second)
	a.FleetAlert = true
	b := trial("x", "shallow", "remediate", true, time.Second)
	if r := Summarize([]TrialResult{a, b})[0]; r.FleetAlerts != "1/2" {
		t.Fatalf("FleetAlerts = %q", r.FleetAlerts)
	}
}

func TestRenderMarkdownColumns(t *testing.T) {
	out := RenderMarkdown(Summarize([]TrialResult{trial("x", "shallow", "detect", true, time.Second)}), 0)
	want := "| Fault | HC caught (shallow) | HC caught (deep) | Graywatch caught | Median TTD | Category right | False-positive hosts | p99 detect-only | p99 remediate |"
	if !slices.Contains(strings.Split(out, "\n"), want) {
		t.Fatalf("no header row %q in:\n%s", want, out)
	}
}

func TestRenderMarkdownRow(t *testing.T) {
	a := trial("pool-leak", "shallow", "detect", true, 3*time.Second)
	a.CategoryOK, a.P99 = true, 1003*time.Millisecond
	b := trial("pool-leak", "shallow", "remediate", true, 3*time.Second)
	b.CategoryOK, b.P99 = true, 26*time.Millisecond
	c := trial("pool-leak", "deep", "detect", true, 3*time.Second)
	c.CategoryOK, c.HCDetected = true, true
	out := RenderMarkdown(Summarize([]TrialResult{a, b, c}), 0)
	want := "| pool-leak | 0/1 | 1/1 | 3/3 | 3.0 s | 3/3 | 0 | 1003 ms | 26 ms |"
	if !strings.Contains(out, want) {
		t.Fatalf("no row %q in:\n%s", want, out)
	}
}

func TestRenderMarkdownMarksWhatWasNotMeasured(t *testing.T) {
	out := RenderMarkdown(Summarize([]TrialResult{trial("x", "shallow", "detect", false, 0)}), 0)
	want := "| x | 0/1 | n/a | 0/1 | n/a | 0/1 | 0 | 0 ms | n/a |"
	if !strings.Contains(out, want) {
		t.Fatalf("no row %q in:\n%s", want, out)
	}
}

func TestRenderMarkdownStatesTheFalsePositiveRate(t *testing.T) {
	out := RenderMarkdown(nil, 0.25)
	if !strings.Contains(out, "0.25 per host-hour") {
		t.Fatalf("no rate in:\n%s", out)
	}
}

func TestRenderMarkdownHasAnErrorTable(t *testing.T) {
	a := trial("x", "shallow", "detect", true, time.Second)
	a.ErrRate, a.FleetAlert = 0.0494, true
	b := trial("x", "shallow", "remediate", true, time.Second)
	b.ErrRate = 0.0076
	c := trial("x", "deep", "detect", true, time.Second)
	c.ErrRate = 0.0046
	out := RenderMarkdown(Summarize([]TrialResult{a, b, c}), 0)
	for _, want := range []string{
		"| Fault | Errors detect-only | Errors remediate | Errors deep health check | Fleet alerts |",
		"| x | 4.94% | 0.76% | 0.46% | 1/3 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestFalsePositiveRate(t *testing.T) {
	none := func(hosts int) TrialResult {
		return TrialResult{Scenario: "none", Mode: "detect", Health: "shallow", FalsePosHosts: hosts}
	}
	rs := []TrialResult{none(1), none(0), trial("x", "shallow", "detect", true, time.Second)}
	rate, hostHours, err := FalsePositiveRate(rs, 20, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if hostHours != 10 {
		t.Errorf("hostHours = %v, want 2 x 20 x 0.25", hostHours)
	}
	if math.Abs(rate-0.1) > 1e-9 {
		t.Errorf("rate = %v, want 0.1", rate)
	}
}

func TestFalsePositiveRateNeedsABaseline(t *testing.T) {
	rs := []TrialResult{trial("x", "shallow", "detect", true, time.Second)}
	if _, _, err := FalsePositiveRate(rs, 20, time.Minute); err == nil {
		t.Fatal("want error: a rate of 0 would claim something that was never measured")
	}
}

func TestReadTrialsCSVRoundTrip(t *testing.T) {
	in := []TrialResult{{
		Scenario: "pool-leak", Mode: "detect", Health: "deep", Target: "app-07", Trial: 2,
		HCDetected: true, HCTTD: 8 * time.Second, GWDetected: true, GWTTD: 3 * time.Second,
		GWCategory: "app", CategoryOK: true, FalsePosHosts: 1, Drains: 2, FleetAlert: true,
		P50: 9 * time.Millisecond, P99: 1003 * time.Millisecond, ErrRate: 0.0494, BaselineP99: 21 * time.Millisecond,
	}}
	var buf bytes.Buffer
	if err := WriteTrialsCSV(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadTrialsCSV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(in, out) {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestReadTrialsCSVRejectsBadInput(t *testing.T) {
	var good bytes.Buffer
	if err := WriteTrialsCSV(&good, nil); err != nil {
		t.Fatal(err)
	}
	header := strings.TrimSpace(good.String())
	for name, body := range map[string]string{
		"empty":        "",
		"wrong header": "scenario,mode\npool-leak,detect\n",
		"bad bool":     header + "\npool-leak,detect,shallow,app-07,1,maybe,0,true,0,app,true,0,0,false,1,1,0,1\n",
		"bad number":   header + "\npool-leak,detect,shallow,app-07,x,false,0,true,0,app,true,0,0,false,1,1,0,1\n",
		"short row":    header + "\npool-leak,detect\n",
	} {
		if _, err := ReadTrialsCSV(strings.NewReader(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestReportAddsNotesAndBaseline(t *testing.T) {
	none := TrialResult{Scenario: "none", Mode: "detect", Health: "shallow"}
	out, err := Report([]TrialResult{none, trial("x", "shallow", "detect", true, time.Second)}, 20, 11*time.Minute, "## How to read this\nHand-written.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Results",
		"bench report",
		"| x | 0/1 |",
		"3.67 host-hours",
		"## How to read this",
		"Hand-written.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}
