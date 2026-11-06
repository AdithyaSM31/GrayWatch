package remediate

import "testing"

// A server a person took out is theirs, however sick it looks. Draining it would make
// it Graywatch's, and Graywatch would later put it back into traffic.
func TestServerInMaintenanceIsNeverDrained(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "MAINT"))
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 10}))
	if len(got) != 0 {
		t.Fatalf("actions = %q", kinds(got))
	}
}

func TestServerInDrainIsNeverDrained(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DRAIN"))
	if got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 10})); len(got) != 0 {
		t.Fatalf("actions = %q", kinds(got))
	}
}

// The worst score isn't always one that can be acted on.
func TestSuspectInMaintenanceDoesNotHideTheNextOne(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "MAINT"))
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 50, "app-03": 10}))
	wantActions(t, got, "drain app-03")
}

// A server the health check took out gets no traffic already. If it comes back and is
// still suspect, that is when to drain it.
func TestSuspectThatIsDownIsDrainedOnceItIsBack(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-03", "DOWN"))
	if got := pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})); len(got) != 0 {
		t.Fatalf("while down: actions = %q", kinds(got))
	}
	pl.Reconcile(stats("app-03", "UP"))
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "drain app-03")
}
