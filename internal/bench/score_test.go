package bench

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

var (
	t0    = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	tBase = t0.Add(-30 * time.Second)
	t1    = t0.Add(90 * time.Second)
)

const scoreTarget = "app-07"

func leakScenario() Scenario {
	return Scenario{
		Name: "pool-leak", Fault: "pool-leak", ExpectedCategory: "app", Target: TargetRandom,
		Baseline: 30 * time.Second, FaultDur: 90 * time.Second, Cooldown: 30 * time.Second,
	}
}

func suspect(at time.Time, host, category string) events.Event {
	return events.Event{TS: at, Type: events.Suspect, Host: host, Category: category}
}

func score(evs []events.Event, hc []HCObs, reqs []loadgen.Result) TrialResult {
	return Score(leakScenario(), scoreTarget, tBase, t0, t1, evs, hc, reqs)
}

func TestScoreDetectionAndTTD(t *testing.T) {
	r := score([]events.Event{suspect(t0.Add(6*time.Second), scoreTarget, "app")}, nil, nil)
	if !r.GWDetected || r.GWTTD != 6*time.Second {
		t.Fatalf("detected %v, TTD %v, want true, 6s", r.GWDetected, r.GWTTD)
	}
	if r.Scenario != "pool-leak" || r.Target != scoreTarget {
		t.Fatalf("result = %+v", r)
	}
}

func TestScoreUsesFirstSuspect(t *testing.T) {
	r := score([]events.Event{
		suspect(t0.Add(40*time.Second), scoreTarget, "cpu"),
		suspect(t0.Add(6*time.Second), scoreTarget, "app"),
	}, nil, nil)
	if r.GWTTD != 6*time.Second || r.GWCategory != "app" {
		t.Fatalf("TTD %v, category %q", r.GWTTD, r.GWCategory)
	}
}

func TestScoreIgnoresDetectionBeforeFault(t *testing.T) {
	r := score([]events.Event{suspect(t0.Add(-10*time.Second), scoreTarget, "app")}, nil, nil)
	if r.GWDetected {
		t.Fatal("a suspect before the fault is not a detection")
	}
	if r.FalsePosHosts != 1 {
		t.Fatalf("FalsePosHosts = %d, want 1", r.FalsePosHosts)
	}
}

func TestScoreIgnoresDetectionAfterRevert(t *testing.T) {
	r := score([]events.Event{suspect(t1.Add(time.Second), scoreTarget, "app")}, nil, nil)
	if r.GWDetected {
		t.Fatal("a suspect after the revert is not a detection")
	}
	if r.FalsePosHosts != 0 {
		t.Fatalf("FalsePosHosts = %d, the target after t0 is not a false positive", r.FalsePosHosts)
	}
}

func TestScoreWindowEdgesAreInclusive(t *testing.T) {
	for _, at := range []time.Time{t0, t1} {
		if r := score([]events.Event{suspect(at, scoreTarget, "app")}, nil, nil); !r.GWDetected {
			t.Errorf("suspect at %v not detected", at)
		}
	}
}

func TestScoreHealthCheck(t *testing.T) {
	r := score(nil, []HCObs{
		{At: t0.Add(-5 * time.Second), Server: scoreTarget, Down: true},
		{At: t0.Add(4 * time.Second), Server: "app-02", Down: true},
		{At: t0.Add(6 * time.Second), Server: scoreTarget, Down: false},
		{At: t0.Add(8 * time.Second), Server: scoreTarget, Down: true},
	}, nil)
	if !r.HCDetected || r.HCTTD != 8*time.Second {
		t.Fatalf("HC detected %v, TTD %v, want true, 8s", r.HCDetected, r.HCTTD)
	}
}

func TestScoreHealthCheckMissed(t *testing.T) {
	if r := score(nil, nil, nil); r.HCDetected || r.GWDetected {
		t.Fatalf("result = %+v", r)
	}
}

func TestScoreFalsePositives(t *testing.T) {
	r := score([]events.Event{
		suspect(t0.Add(5*time.Second), "app-02", "cpu"),
		suspect(t0.Add(9*time.Second), "app-02", "cpu"),
		suspect(t0.Add(20*time.Second), "app-11", "cpu"),
		suspect(tBase.Add(-time.Second), "app-12", "cpu"),                                // before the trial
		suspect(t1.Add(30*time.Second+time.Second), "app-13", "cpu"),                     // after the trial
		{TS: t0.Add(25 * time.Second), Type: events.Healthy, Host: "app-14"},             // not a suspect
		{TS: t0.Add(26 * time.Second), Type: events.Drain, Host: "app-15", DryRun: true}, // not a suspect
	}, nil, nil)
	if r.FalsePosHosts != 2 {
		t.Fatalf("FalsePosHosts = %d, want 2", r.FalsePosHosts)
	}
}

func TestScoreLatencyWindow(t *testing.T) {
	req := func(at time.Time, latency time.Duration, status int) loadgen.Result {
		return loadgen.Result{Scheduled: at, Latency: latency, Status: status}
	}
	reqs := []loadgen.Result{
		req(tBase.Add(-time.Second), time.Hour, 200), // before the trial
		req(tBase, 7*time.Millisecond, 200),
		req(t0.Add(-time.Millisecond), 8*time.Millisecond, 200),
		req(t1.Add(time.Millisecond), time.Hour, 200), // after the fault
	}
	for i := 0; i < 99; i++ {
		reqs = append(reqs, req(t0.Add(time.Second), 10*time.Millisecond, 200))
	}
	reqs = append(reqs, req(t1, 2*time.Second, 0))

	r := score(nil, nil, reqs)
	if r.P50 != 10*time.Millisecond || r.P99 != 10*time.Millisecond {
		t.Errorf("P50 %v, P99 %v, want 10ms, 10ms", r.P50, r.P99)
	}
	if r.ErrRate != 0.01 {
		t.Errorf("ErrRate = %v, want 0.01", r.ErrRate)
	}
	if r.BaselineP99 != 8*time.Millisecond {
		t.Errorf("BaselineP99 = %v, want 8ms", r.BaselineP99)
	}
}

func TestScoreCategory(t *testing.T) {
	r := score([]events.Event{suspect(t0.Add(time.Second), scoreTarget, "app")}, nil, nil)
	if r.GWCategory != "app" || !r.CategoryOK {
		t.Fatalf("category %q, ok %v", r.GWCategory, r.CategoryOK)
	}
	r = score([]events.Event{suspect(t0.Add(time.Second), scoreTarget, "disk")}, nil, nil)
	if r.GWCategory != "disk" || r.CategoryOK {
		t.Fatalf("category %q, ok %v", r.GWCategory, r.CategoryOK)
	}
	if r = score(nil, nil, nil); r.CategoryOK {
		t.Fatal("a miss cannot have the right category")
	}
}

func TestScoreCountsDrainsOfTheTargetOnly(t *testing.T) {
	r := score([]events.Event{
		{TS: t0.Add(5 * time.Second), Type: events.Drain, Host: scoreTarget},
		{TS: t0.Add(70 * time.Second), Type: events.Drain, Host: scoreTarget, DryRun: true},
		{TS: t0.Add(6 * time.Second), Type: events.Drain, Host: "app-03"},
		{TS: t0.Add(-6 * time.Second), Type: events.Drain, Host: scoreTarget},
	}, nil, nil)
	if r.Drains != 2 {
		t.Fatalf("Drains = %d, want 2", r.Drains)
	}
}

func depScenario() Scenario {
	sc := leakScenario()
	sc.Name, sc.Fault, sc.ExpectedCategory, sc.Target = "dep-slow", "dep-slow", "fleet", TargetDep
	return sc
}

func TestScoreDepScenario(t *testing.T) {
	r := Score(depScenario(), "dep", tBase, t0, t1, []events.Event{
		{TS: t0.Add(2 * time.Second), Type: events.FleetAlert, Reason: "median latency"},
	}, nil, nil)
	if !r.FleetAlert || r.Drains != 0 {
		t.Fatalf("FleetAlert %v, Drains %d", r.FleetAlert, r.Drains)
	}
	if !r.GWDetected || r.GWTTD != 2*time.Second || r.GWCategory != "fleet" || !r.CategoryOK {
		t.Fatalf("a fleet alert is how this fault is caught: %+v", r)
	}
}

func TestScoreDepScenarioCountsDrainsOnAllHosts(t *testing.T) {
	r := Score(depScenario(), "dep", tBase, t0, t1, []events.Event{
		{TS: t0.Add(3 * time.Second), Type: events.Drain, Host: "app-03"},
		{TS: t0.Add(4 * time.Second), Type: events.Drain, Host: "app-11"},
	}, nil, nil)
	if r.Drains != 2 {
		t.Fatalf("Drains = %d, want 2", r.Drains)
	}
}

func TestScoreDepScenarioAnyDownServerIsAHealthCheckDetection(t *testing.T) {
	r := Score(depScenario(), "dep", tBase, t0, t1, nil, []HCObs{
		{At: t0.Add(9 * time.Second), Server: "app-12", Down: true},
	}, nil)
	if !r.HCDetected || r.HCTTD != 9*time.Second {
		t.Fatalf("HC detected %v, TTD %v", r.HCDetected, r.HCTTD)
	}
}

func TestScoreFleetAlertWindowIncludesCooldown(t *testing.T) {
	in := score([]events.Event{{TS: t1.Add(29 * time.Second), Type: events.FleetAlert}}, nil, nil)
	if !in.FleetAlert {
		t.Error("an alert caused by the revert belongs to this trial")
	}
	before := score([]events.Event{{TS: t0.Add(-time.Second), Type: events.FleetAlert}}, nil, nil)
	if before.FleetAlert {
		t.Error("an alert before the fault is not caused by it")
	}
}

func TestScoreNoneScenario(t *testing.T) {
	sc := Scenario{Name: "none", Target: TargetNone, Baseline: 30 * time.Second, FaultDur: 600 * time.Second, Cooldown: 30 * time.Second}
	r := Score(sc, "", tBase, t0, t0.Add(600*time.Second), []events.Event{
		suspect(t0.Add(100*time.Second), "app-05", "cpu"),
	}, nil, nil)
	if r.GWDetected || r.FalsePosHosts != 1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestWriteTrialsCSV(t *testing.T) {
	var buf bytes.Buffer
	err := WriteTrialsCSV(&buf, []TrialResult{{
		Scenario: "pool-leak", Mode: "detect", Health: "shallow", Target: "app-07", Trial: 2,
		HCDetected: false, GWDetected: true, GWTTD: 6 * time.Second, GWCategory: "app", CategoryOK: true,
		FalsePosHosts: 1, Drains: 3, FleetAlert: true,
		P50: 10 * time.Millisecond, P99: 1500 * time.Millisecond, ErrRate: 0.25, BaselineP99: 15 * time.Millisecond,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	wantHeader := "scenario,mode,health,target,trial,hc_detected,hc_ttd_ms,gw_detected,gw_ttd_ms,gw_category,category_ok,false_pos_hosts,drains,fleet_alert,p50_ms,p99_ms,err_rate,baseline_p99_ms"
	if got := strings.Join(rows[0], ","); got != wantHeader {
		t.Errorf("header = %s", got)
	}
	wantRow := "pool-leak,detect,shallow,app-07,2,false,0,true,6000,app,true,1,3,true,10,1500,0.25,15"
	if got := strings.Join(rows[1], ","); got != wantRow {
		t.Errorf("row = %s", got)
	}
}

func TestSaveTrialsCSVReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trials.csv")
	if err := SaveTrialsCSV(path, []TrialResult{{Scenario: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveTrialsCSV(path, []TrialResult{{Scenario: "a"}, {Scenario: "b"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 3 {
		t.Fatalf("%d lines, want a header and 2 rows:\n%s", got, data)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestWriteTrialsCSVEmptyStillHasHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTrialsCSV(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "scenario,") {
		t.Fatalf("got %q", buf.String())
	}
}
