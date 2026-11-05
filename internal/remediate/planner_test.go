package remediate

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/detect"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func newPlanner(p Policy, n int, drained ...string) *Planner {
	return NewPlanner(p, n, drained, t0.Add(-time.Hour))
}

// obsAt builds an observation of hosts app-01..app-20. Each suspect's score is
// given in the map; everyone else is healthy.
func obsAt(at time.Time, suspects map[string]float64) detect.Observation {
	obs := detect.Observation{At: at}
	for i := 1; i <= 20; i++ {
		host := fmt.Sprintf("app-%02d", i)
		v := detect.Verdict{Host: host, State: detect.Healthy}
		if score, ok := suspects[host]; ok {
			v.State, v.Score = detect.Suspect, score
		}
		obs.Verdicts = append(obs.Verdicts, v)
	}
	return obs
}

func kinds(actions []Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, string(a.Kind)+" "+a.Host)
	}
	return out
}

func wantActions(t *testing.T, got []Action, want ...string) {
	t.Helper()
	if !slices.Equal(kinds(got), want) {
		t.Fatalf("actions = %q, want %q", kinds(got), want)
	}
}

// commit plans and commits everything planned, as the daemon would on success.
func commit(pl *Planner, obs detect.Observation) []Action {
	actions := pl.Plan(obs)
	for _, a := range actions {
		pl.Commit(a, obs.At, nil)
	}
	return actions
}

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if p.MaxDrainFrac != 0.10 || p.FleetWideFrac != 0.30 || p.Cooldown != 60*time.Second ||
		p.MinActionGap != 10*time.Second || p.FlapWindow != 10*time.Minute || p.MaxDrainsPerWindow != 3 || p.DryRun {
		t.Fatalf("policy = %+v", p)
	}
}

func TestDrainsSuspect(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 10})), "drain app-03")
}

func TestNoActionWhenAllHealthy(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	wantActions(t, pl.Plan(obsAt(t0, nil)))
}

func TestDrainPicksHighestScore(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	got := pl.Plan(obsAt(t0, map[string]float64{"app-03": 10, "app-09": 50, "app-05": 20}))
	wantActions(t, got, "drain app-09")
}

func TestEqualScoresPickDeterministically(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	got := pl.Plan(obsAt(t0, map[string]float64{"app-09": 10, "app-03": 10}))
	wantActions(t, got, "drain app-03")
}

func TestActionsCarryAReason(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	got := pl.Plan(obsAt(t0, map[string]float64{"app-03": 10}))
	if got[0].Reason == "" {
		t.Fatal("no reason")
	}
}

func TestCapsConcurrentDrains(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-01", "app-02")
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 9, "app-02": 9, "app-03": 9}))
	wantActions(t, got, "cap_reached app-03")
}

func TestCapAllowsUpToTheLimit(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-01")
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 9, "app-03": 9}))
	wantActions(t, got, "drain app-03")
}

func TestCapIsAtLeastOne(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 5)
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-01": 9})), "drain app-01")

	// 10% of 9 rounds down to 0, and the floor of 1 still applies. It is 9 rather
	// than 5 because 2 suspects out of 5 would pass the 30% fleet-wide limit.
	pl = newPlanner(DefaultPolicy(), 9, "app-02")
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-01": 9, "app-02": 9})), "cap_reached app-01")
}

// 0.1*30 is 3.0000000000000004 and 0.07*100 is 7.000000000000001, but the
// floating point error can fall either way, so the cap must not lose a host.
func TestCapSurvivesFloatingPointError(t *testing.T) {
	p := DefaultPolicy()
	p.MaxDrainFrac = 0.29
	pl := newPlanner(p, 100, "a", "b")
	if got := pl.cap(); got != 29 {
		t.Fatalf("cap = %d, want 29", got)
	}
}

func TestDrainedSuspectIsNotDrainedAgain(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-03")
	wantActions(t, pl.Plan(obsAt(t0, map[string]float64{"app-03": 99})))
}

func TestFleetWideSuspicionBlocksDrains(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	suspects := map[string]float64{}
	for i := 1; i <= 7; i++ {
		suspects[fmt.Sprintf("app-%02d", i)] = 9
	}
	wantActions(t, pl.Plan(obsAt(t0, suspects)), "fleet_wide ")
}

func TestExactlyThirtyPercentStillDrains(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	suspects := map[string]float64{}
	for i := 1; i <= 6; i++ {
		suspects[fmt.Sprintf("app-%02d", i)] = 9
	}
	wantActions(t, pl.Plan(obsAt(t0, suspects)), "drain app-01")
}

func TestFleetAlertBlocksDrains(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	obs := obsAt(t0, map[string]float64{"app-03": 10})
	obs.Fleet = &detect.FleetAlert{Reason: "median latency"}
	got := pl.Plan(obs)
	wantActions(t, got, "fleet_wide ")
	if got[0].Reason != "median latency" {
		t.Fatalf("reason = %q", got[0].Reason)
	}
}

// Putting a host back into a sick fleet would hide what is going on.
func TestFleetAlertBlocksRestores(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	obs := obsAt(t0.Add(2*time.Minute), nil)
	obs.Fleet = &detect.FleetAlert{Reason: "x"}
	wantActions(t, pl.Plan(obs), "fleet_wide ")
}

func TestMinActionGap(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	next := map[string]float64{"app-03": 10, "app-04": 10}
	wantActions(t, pl.Plan(obsAt(t0.Add(5*time.Second), next)))
	wantActions(t, pl.Plan(obsAt(t0.Add(10*time.Second), next)), "drain app-04")
}

func TestRestoreAfterCooldown(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	wantActions(t, pl.Plan(obsAt(t0.Add(59*time.Second), nil)))
	wantActions(t, pl.Plan(obsAt(t0.Add(61*time.Second), nil)), "restore app-03")
}

func TestRestoreClearsDrainedAndFreesTheCap(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	commit(pl, obsAt(t0.Add(61*time.Second), nil))
	if pl.Drained()["app-03"] {
		t.Fatal("app-03 still drained")
	}
}

func TestNoRestoreWhileSuspect(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	wantActions(t, pl.Plan(obsAt(t0.Add(5*time.Minute), map[string]float64{"app-03": 10})))
}

// A host with no verdict is no evidence of health.
func TestNoRestoreWithoutAVerdict(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	wantActions(t, pl.Plan(detect.Observation{At: t0.Add(5 * time.Minute)}))
}

func TestAtMostOneActionPerPlan(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	wantActions(t, pl.Plan(obsAt(t0.Add(2*time.Minute), map[string]float64{"app-05": 10})), "restore app-03")
}

// drainCycle drains and restores app-03 n times, a minute apart, and returns the
// time of the last drain. The last drain is left in place.
func drainCycle(t *testing.T, pl *Planner, n int) time.Time {
	t.Helper()
	at := t0
	for i := 0; i < n; i++ {
		wantActions(t, commit(pl, obsAt(at, map[string]float64{"app-03": 10})), "drain app-03")
		if i == n-1 {
			return at
		}
		at = at.Add(61 * time.Second)
		wantActions(t, commit(pl, obsAt(at, nil)), "restore app-03")
		at = at.Add(11 * time.Second)
	}
	return at
}

func TestFlapProtection(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	last := drainCycle(t, pl, 3)
	at := last.Add(61 * time.Second)

	wantActions(t, commit(pl, obsAt(at, nil)), "flapping app-03")
	wantActions(t, pl.Plan(obsAt(at.Add(time.Second), nil)))
	if !pl.Drained()["app-03"] {
		t.Fatal("flapping host was restored")
	}
}

func TestFlappingHostRestoredOnceWindowPasses(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	last := drainCycle(t, pl, 3)
	wantActions(t, commit(pl, obsAt(last.Add(61*time.Second), nil)), "flapping app-03")
	// The first drain leaves the 10 minute window, leaving 2 of 3.
	wantActions(t, pl.Plan(obsAt(t0.Add(11*time.Minute), nil)), "restore app-03")
}

func TestTwoDrainsAreNotFlapping(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	last := drainCycle(t, pl, 2)
	wantActions(t, pl.Plan(obsAt(last.Add(61*time.Second), nil)), "restore app-03")
}

// A flapping host stays out, and that must not stop other hosts being handled.
func TestFlapAlertDoesNotBlockOtherHosts(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	last := drainCycle(t, pl, 3)
	got := pl.Plan(obsAt(last.Add(61*time.Second), map[string]float64{"app-09": 10}))
	wantActions(t, got, "flapping app-03", "drain app-09")
}

func TestPreexistingDrainsCountTowardCap(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-01", "app-02")
	if !pl.Drained()["app-01"] || !pl.Drained()["app-02"] {
		t.Fatalf("drained = %v", pl.Drained())
	}
	got := pl.Plan(obsAt(t0, map[string]float64{"app-01": 9, "app-02": 9, "app-05": 9}))
	wantActions(t, got, "cap_reached app-05")
}

// After a daemon restart nothing is known about a drained host yet: the detector
// starts it as Healthy with no history. Its cooldown must start at startup, or it
// would go straight back into rotation on the first tick while still sick.
func TestPreexistingDrainCooldownStartsAtStartup(t *testing.T) {
	pl := NewPlanner(DefaultPolicy(), 20, []string{"app-03"}, t0)
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Second), nil)))
	wantActions(t, pl.Plan(obsAt(t0.Add(59*time.Second), nil)))
	wantActions(t, pl.Plan(obsAt(t0.Add(61*time.Second), nil)), "restore app-03")
}

// Review Focus 4.
func TestActuatorFailureNotCountedAsDrained(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	obs := obsAt(t0, map[string]float64{"app-03": 10})
	actions := pl.Plan(obs)
	pl.Commit(actions[0], t0, errors.New("runtime API down"))
	if pl.Drained()["app-03"] {
		t.Fatal("failed drain recorded as drained")
	}
	// No MinActionGap wait either: the retry is on the very next tick.
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Second), map[string]float64{"app-03": 10})), "drain app-03")
}

func TestFailedDrainsDoNotUseUpTheCap(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-01")
	for i := 0; i < 5; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		actions := pl.Plan(obsAt(at, map[string]float64{"app-01": 10, "app-03": 10}))
		wantActions(t, actions, "drain app-03")
		pl.Commit(actions[0], at, errors.New("down"))
	}
	if got := len(pl.Drained()); got != 1 {
		t.Fatalf("drained %d hosts, want 1", got)
	}
}

func TestFailedRestoreStaysDrained(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	commit(pl, obsAt(t0, map[string]float64{"app-03": 10}))
	at := t0.Add(61 * time.Second)
	actions := pl.Plan(obsAt(at, nil))
	pl.Commit(actions[0], at, errors.New("down"))
	if !pl.Drained()["app-03"] {
		t.Fatal("failed restore recorded as restored")
	}
}

func TestDryRunMarksActionsAndCommitsNothing(t *testing.T) {
	p := DefaultPolicy()
	p.DryRun = true
	pl := newPlanner(p, 20)
	obs := obsAt(t0, map[string]float64{"app-03": 10})
	actions := pl.Plan(obs)
	wantActions(t, actions, "drain app-03")
	if !actions[0].DryRun {
		t.Fatal("action not marked as dry run")
	}
	pl.Commit(actions[0], t0, nil)
	if len(pl.Drained()) != 0 {
		t.Fatalf("drained = %v after a dry run", pl.Drained())
	}
	// Nothing changed, so the gap does not apply to the next tick either.
	wantActions(t, pl.Plan(obsAt(t0.Add(time.Second), map[string]float64{"app-03": 10})), "drain app-03")
}

func TestPlanDoesNotChangeState(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20)
	obs := obsAt(t0, map[string]float64{"app-03": 10})
	first := kinds(pl.Plan(obs))
	second := kinds(pl.Plan(obs))
	if !slices.Equal(first, second) {
		t.Fatalf("Plan changed state: %q then %q", first, second)
	}
	if len(pl.Drained()) != 0 {
		t.Fatal("Plan drained a host")
	}
}

func TestDrainedReturnsACopy(t *testing.T) {
	pl := newPlanner(DefaultPolicy(), 20, "app-01")
	pl.Drained()["app-99"] = true
	if pl.Drained()["app-99"] {
		t.Fatal("caller changed the planner's state")
	}
}
