package remediate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/haproxy"
)

func stats(pairs ...string) []haproxy.ServerStat {
	var out []haproxy.ServerStat
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, haproxy.ServerStat{Name: pairs[i], Status: pairs[i+1]})
	}
	return out
}

// HAProxy restarted, or someone set the server back: the planner must stop calling
// it drained, or a host still failing traffic is never drained again.
func TestReconcileForgetsAServerBackInRotation(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	pl.Reconcile(stats("app-03", "UP"))
	if pl.Drained()["app-03"] {
		t.Fatalf("drained = %v", pl.Drained())
	}
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Minute), map[string]float64{"app-03": 10})), "drain app-03")
}

func TestReconcileKeepsOurOwnDrains(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	pl.Reconcile(stats("app-03", "MAINT", "app-04", "UP"))
	if !pl.Drained()["app-03"] {
		t.Fatalf("drained = %v", pl.Drained())
	}
}

// A server out of rotation that the planner did not drain still uses up the
// fleet's margin, such as one left in DRAIN by a rollback that failed.
func TestCapCountsServersTheOthersTookOut(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DRAIN", "app-02", "MAINT"))
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "cap_reached app-03")
}

func TestCapCountsServersTheHealthCheckTookOut(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DOWN", "app-02", "DOWN 1/2", "app-04", "UP"))
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "cap_reached app-03")
}

func TestCapStillHasRoomBelowTheLimit(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DOWN"))
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "drain app-03")
}

// One slot, not two: our drain reads MAINT, and that is the same server.
func TestCapDoesNotCountOurOwnDrainTwice(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	pl.Reconcile(stats("app-03", "MAINT"))
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Minute), map[string]float64{"app-05": 10, "app-03": 10})), "drain app-05")
}

// A suspect that is already DOWN uses no extra slot if the planner drains it, since
// drained and down are the same server.
func TestCapDoesNotCountADownServerAlreadyDrained(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	pl.Reconcile(stats("app-03", "DOWN"))
	if !pl.Drained()["app-03"] {
		t.Fatal("a drained server that reads DOWN is still out of rotation")
	}
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Minute), map[string]float64{"app-05": 10, "app-03": 10})), "drain app-05")
}

// Hosts the health check already took out get no traffic, so they read healthy to
// the detector. Half the fleet out must still count as a fleet-wide problem.
func TestFleetWideCheckCountsDownServers(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DOWN", "app-02", "DOWN", "app-04", "DOWN", "app-05", "DOWN", "app-06", "DOWN"))
	actions := pl.Plan(obsAt(t0, map[string]float64{"app-07": 10, "app-08": 10}))
	wantActions(t, actions, "fleet_wide ")
	if !strings.Contains(actions[0].Reason, "7 of 20") {
		t.Fatalf("reason = %q", actions[0].Reason)
	}
}

func TestFleetWideCheckCountsASuspectThatIsDownOnce(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DOWN", "app-02", "DOWN", "app-04", "DOWN", "app-05", "DOWN", "app-06", "DOWN"))
	// 6 of 20 is exactly 30%, which is allowed. Counted twice it would be 7.
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 10, "app-07": 10}))
	if slices.Contains(kinds(got), "fleet_wide ") {
		t.Fatalf("actions = %q", kinds(got))
	}
}

// A later read replaces the earlier one.
func TestReconcileReplacesTheLastReading(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	pl.Reconcile(stats("app-01", "DOWN", "app-02", "MAINT"))
	pl.Reconcile(stats("app-01", "UP", "app-02", "UP"))
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "drain app-03")
}

// A dry run changes nothing, so a restore it proposes never completes. It must not
// hide every drain behind it.
func TestDryRunRestoreDoesNotStarveDrains(t *testing.T) {
	p := DefaultPolicy()
	p.DryRun = true
	pl := NewPlanner(p, 20, []string{"app-05"}, t0)
	got := pl.Plan(obsAt(t0.Add(2*time.Minute), map[string]float64{"app-03": 10}))
	wantActions(t, got, "restore app-05", "drain app-03")
}

// A restore that keeps failing must not hide drains either, or be retried every tick.
func TestFailedRestoreBacksOffAndLetsDrainsThrough(t *testing.T) {
	pl := NewPlanner(DefaultPolicy(), 20, []string{"app-05"}, t0)
	at := t0.Add(2 * time.Minute)
	first := pl.Plan(obsAt(at, map[string]float64{"app-03": 10}))
	wantActions(t, first, "restore app-05")
	pl.Commit(first[0], at, errors.New("connection refused"))

	next := at.Add(time.Second)
	wantActions(t, pl.Plan(obsAt(next, map[string]float64{"app-03": 10})), "drain app-03")

	later := at.Add(pl.policy.MinActionGap + time.Second)
	wantActions(t, pl.Plan(obsAt(later, nil)), "restore app-05")
}

func TestSuccessfulRestoreClearsTheBackoff(t *testing.T) {
	pl := NewPlanner(DefaultPolicy(), 20, []string{"app-05"}, t0)
	at := t0.Add(2 * time.Minute)
	a := pl.Plan(obsAt(at, nil))
	pl.Commit(a[0], at, errors.New("connection refused"))
	later := at.Add(pl.policy.MinActionGap + time.Second)
	a = pl.Plan(obsAt(later, nil))
	wantActions(t, a, "restore app-05")
	pl.Commit(a[0], later, nil)
	if len(pl.retryAt) != 0 {
		t.Fatalf("retryAt = %v", pl.retryAt)
	}
}

// The usual way this goes wrong is SIGTERM during the wait: both later commands
// would fail on the cancelled context and leave the server in DRAIN.
func TestDrainFinishesItsWorkAfterCancellation(t *testing.T) {
	f := newFakeRuntime(t, 5)
	a := newActuator(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Drain(ctx, "app-03") }()
	for !slices.Contains(f.got(), setDrain) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return")
	}
	// A status read cancelled mid-flight can reach the fake after this, so the order
	// of the state changes is what to check, not the last command overall.
	if got := f.got(); !slices.Contains(got, setMaint) || slices.Contains(got, setReady) {
		t.Fatalf("commands = %q, want the drain finished", got)
	}
}

func TestDrainRollbackRunsAfterCancellation(t *testing.T) {
	f := newFakeRuntime(t, 5)
	f.setReply = func(cmd string) string {
		if cmd == setMaint {
			return "failed\n"
		}
		return "\n"
	}
	a := newActuator(f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !slices.Contains(f.got(), setDrain) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := a.Drain(ctx, "app-03"); err == nil {
		t.Fatal("want error")
	}
	got := f.got()
	maint, ready := slices.Index(got, setMaint), slices.Index(got, setReady)
	if maint < 0 || ready < maint {
		t.Fatalf("commands = %q, want it put back after the failed maint", got)
	}
}
