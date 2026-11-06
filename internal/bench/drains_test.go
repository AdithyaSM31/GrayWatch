package bench

import (
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/events"
)

// In a dry run nothing changes, so the same would-be drain can be logged again after
// a fleet alert. It is still one drain that would have happened.
func TestScoreCountsADryRunDrainOncePerHost(t *testing.T) {
	r := score([]events.Event{
		{TS: t0.Add(3 * time.Second), Type: events.Drain, Host: scoreTarget, DryRun: true},
		{TS: t0.Add(70 * time.Second), Type: events.Drain, Host: scoreTarget, DryRun: true},
	}, nil, nil)
	if r.Drains != 1 {
		t.Fatalf("Drains = %d, want 1", r.Drains)
	}
}

func TestScoreCountsRealDrainsEachTime(t *testing.T) {
	r := score([]events.Event{
		{TS: t0.Add(3 * time.Second), Type: events.Drain, Host: scoreTarget},
		{TS: t0.Add(70 * time.Second), Type: events.Drain, Host: scoreTarget},
	}, nil, nil)
	if r.Drains != 2 {
		t.Fatalf("Drains = %d, want 2", r.Drains)
	}
}

// With no fault, any drain at all is a mistake, and it should show.
func TestScoreNoneScenarioCountsAnyDrain(t *testing.T) {
	sc := Scenario{Name: "none", Target: TargetNone, Baseline: 30 * time.Second, FaultDur: 600 * time.Second, Cooldown: 30 * time.Second}
	r := Score(sc, "", tBase, t0, t0.Add(600*time.Second), []events.Event{
		{TS: t0.Add(100 * time.Second), Type: events.Drain, Host: "app-05"},
	}, nil, nil)
	if r.Drains != 1 {
		t.Fatalf("Drains = %d, want 1", r.Drains)
	}
}
