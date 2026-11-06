package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/remediate"
)

func TestStateIsSavedWhenDrainsChange(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	s.d.SaveState = func(st remediate.State) error { saved = append(saved, st); return nil }
	s.run(10, s.healthy)
	if len(saved) != 0 {
		t.Fatalf("saved %d times with nothing to save", len(saved))
	}
	s.run(8, s.leakOn("app-07"))
	if len(saved) != 1 || !slices.Equal(saved[0].Drained, []string{"app-07"}) {
		t.Fatalf("saved = %+v", saved)
	}
	s.run(70, s.healthy)
	if last := saved[len(saved)-1]; len(last.Drained) != 0 {
		t.Fatalf("after the restore, state = %+v", last)
	}
}

func TestStateIsNotSavedInADryRun(t *testing.T) {
	p := remediate.DefaultPolicy()
	p.DryRun = true
	s := newSim(t, p)
	s.d.SaveState = func(remediate.State) error { t.Fatal("a dry run saved state"); return nil }
	s.run(10, s.healthy)
	s.run(10, s.leakOn("app-07"))
}

// HAProxy forgetting a drain changes what must be remembered.
func TestStateIsSavedWhenReconcileForgetsADrain(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	var saved []remediate.State
	s.d.SaveState = func(st remediate.State) error { saved = append(saved, st); return nil }
	s.run(10, s.healthy)
	s.run(8, s.leakOn("app-07"))
	s.d.Servers = func(context.Context) ([]haproxy.ServerStat, error) {
		return []haproxy.ServerStat{{Name: "app-07", Status: "UP"}}, nil
	}
	s.run(1, s.healthy)
	if last := saved[len(saved)-1]; len(last.Drained) != 0 {
		t.Fatalf("state = %+v", last)
	}
}

func TestOwnDrainsAtStartupKeepsOnlyWhatIsStillOut(t *testing.T) {
	got := OwnDrains(remediate.State{Drained: []string{"app-01", "app-02"}}, []haproxy.ServerStat{
		{Name: "app-01", Status: "MAINT"},
		{Name: "app-02", Status: "UP"},
		{Name: "app-03", Status: "MAINT"},
	})
	if !slices.Equal(got, []string{"app-01"}) {
		t.Fatalf("got %q: app-02 is back in rotation, app-03 belongs to someone else", got)
	}
}
