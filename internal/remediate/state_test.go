package remediate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	if err := SaveState(path, pl.State(t0)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Drained, []string{"app-03"}) {
		t.Fatalf("Drained = %q", got.Drained)
	}
	if len(got.History["app-03"]) != 1 || !got.History["app-03"][0].Equal(t0) {
		t.Fatalf("History = %v", got.History)
	}
}

func TestLoadStateMissingFileIsEmpty(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(got.Drained) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLoadStateRejectsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("want error")
	}
}

// A restart must not reset how many times a host has flapped.
func TestLoadedHistoryCountsTowardFlapping(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	drainCycle(t, pl, 3)
	at := t0.Add(3 * time.Minute)
	state := pl.State(at)

	restarted := NewPlanner(DefaultPolicy(), 20, state.Drained, at)
	restarted.LoadHistory(state.History)
	later := at.Add(2 * time.Minute)
	wantActions(t, restarted.Plan(obsAt(later, nil)), "flapping app-03")
}

func TestStateDropsHistoryOlderThanTheFlapWindow(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	state := pl.State(t0.Add(pl.policy.FlapWindow + time.Minute))
	if len(state.History) != 0 {
		t.Fatalf("History = %v", state.History)
	}
}

// Servers out that the planner did not drain are never restored by it.
func TestPlannerLeavesOtherPeoplesMaintenanceAlone(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "MAINT"))
	if got := pl.Plan(obsAt(t0.Add(time.Hour), nil)); len(got) != 0 {
		t.Fatalf("actions = %q", kinds(got))
	}
}
