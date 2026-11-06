package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/remediate"
)

// A drain can take 5 s. A crash in that time must not leave a server out of rotation
// that the next daemon reads as someone else's and never restores.
func TestDrainIsRecordedBeforeTheActuatorRuns(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	s.d.SaveState = func(st remediate.State) error { saved = append(saved, st); return nil }
	checked := false
	s.actuator.fail = func(call string) error {
		checked = true
		if len(saved) == 0 || !slices.Contains(saved[len(saved)-1].Drained, "app-07") {
			t.Errorf("%s ran with state %+v", call, saved)
		}
		return nil
	}
	s.run(10, s.healthy)
	s.run(8, s.leakOn("app-07"))
	if !checked {
		t.Fatal("no drain happened")
	}
}

func TestFailedDrainTakesBackItsRecord(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	s.d.SaveState = func(st remediate.State) error { saved = append(saved, st); return nil }
	s.actuator.fail = func(string) error { return errors.New("runtime API down") }
	s.run(10, s.healthy)
	s.run(8, s.leakOn("app-07"))
	if len(saved) == 0 {
		t.Fatal("nothing saved")
	}
	if last := saved[len(saved)-1]; len(last.Drained) != 0 {
		t.Fatalf("state = %+v after every drain failed", last)
	}
}

// A drain that couldn't be recorded still happens: a sick host comes out, and a host
// out with no record is the safe way round, since it is never restored by mistake.
func TestDrainGoesAheadWhenItsRecordFails(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.d.SaveState = func(remediate.State) error { return errors.New("disk full") }
	s.run(10, s.healthy)
	s.run(8, s.leakOn("app-07"))
	if !s.planner.Drained()["app-07"] {
		t.Fatal("a failed save stopped the drain")
	}
}

func TestFailedSaveIsRetriedOnTheNextTick(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	failing := true
	s.d.SaveState = func(st remediate.State) error {
		if failing {
			return errors.New("disk full")
		}
		saved = append(saved, st)
		return nil
	}
	s.run(10, s.healthy)
	s.run(8, s.leakOn("app-07"))
	failing = false
	s.run(1, s.leakOn("app-07"))
	if len(saved) != 1 || !slices.Equal(saved[0].Drained, []string{"app-07"}) {
		t.Fatalf("saved = %+v, want the drain written once the disk came back", saved)
	}
	s.run(3, s.leakOn("app-07"))
	if len(saved) != 1 {
		t.Fatalf("saved %d times, want no more saves with nothing new", len(saved))
	}
}

// A saved drain that is no longer out must not stay on disk: if a person later put
// that server in maintenance, the next restart would read it as Graywatch's.
func TestSyncStateWritesWhatThePlannerKnows(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	s.d.SaveState = func(st remediate.State) error { saved = append(saved, st); return nil }
	s.d.SyncState()
	if len(saved) != 1 || len(saved[0].Drained) != 0 {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestSyncStateFailureIsRetried(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	calls := 0
	s.d.SaveState = func(remediate.State) error {
		calls++
		if calls == 1 {
			return errors.New("read-only file system")
		}
		return nil
	}
	s.d.SyncState()
	s.run(3, s.healthy)
	if calls != 2 {
		t.Fatalf("saved %d times, want one failure and one retry", calls)
	}
}
