package chaos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// snapshotFault checks that its record exists before it changes anything.
type snapshotFault struct {
	t    *testing.T
	path func() string
}

func (snapshotFault) Name() string     { return "snapshot" }
func (snapshotFault) Category() string { return "test" }
func (snapshotFault) Snapshot(Env, fleet.Host) (map[string]string, error) {
	return map[string]string{"cpu.max": "max 100000"}, nil
}
func (f snapshotFault) Apply(context.Context, Env, fleet.Host) (map[string]string, error) {
	data, err := os.ReadFile(f.path())
	if err != nil {
		f.t.Fatalf("no record before the fault was applied: %v", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		f.t.Fatal(err)
	}
	if s.Saved["cpu.max"] != "max 100000" {
		f.t.Fatalf("record = %+v, want what a revert needs", s)
	}
	return map[string]string{"cpu.max": "max 100000", "extra": "1"}, nil
}
func (snapshotFault) Revert(context.Context, Env, fleet.Host, map[string]string) error { return nil }

// A crash partway through applying a fault must still leave enough to undo it.
func TestApplyRecordsBeforeChangingAnything(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	f := snapshotFault{t: t, path: func() string { return statePath(e, h.Name, "snapshot") }}
	if err := ApplyWithState(context.Background(), e, f, h); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath(e, h.Name, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "extra") {
		t.Fatalf("state = %s, want the final saved values", data)
	}
}

// What a crash midway would leave behind must be enough for revert-all.
func TestEveryFaultCanBeRevertedFromItsSnapshot(t *testing.T) {
	for name, f := range Registry() {
		t.Run(name, func(t *testing.T) {
			e, _ := testEnv(t)
			h := testHost(t, "app-07", 1)
			snap, ok := f.(Snapshotter)
			if !ok {
				t.Fatal("fault cannot be recorded before it is applied")
			}
			saved, err := snap.Snapshot(e, h)
			if err != nil {
				t.Fatal(err)
			}
			final, err := f.Apply(context.Background(), e, h)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range final {
				if saved[key] != want {
					t.Errorf("%s: snapshot %q, apply %q", key, saved[key], want)
				}
			}
			if err := f.Revert(context.Background(), e, h, saved); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// docker restart keeps the container's cgroup path but gives it a new process. The
// saved PID is dead or reused, and running nsenter on it would hit something else.
func TestRevertDropsStateWhenTheContainerRestarted(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 4242)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), h); err != nil {
		t.Fatal(err)
	}
	rec.cmds = nil
	e.SameContainer = func(fleet.Host) bool { return false }
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("ran %q against a process that is gone", rec.cmds)
	}
	if got := stateFiles(t, e); len(got) != 0 {
		t.Fatalf("state files = %q", got)
	}
}

func TestRevertRunsWhenItIsTheSameContainer(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 4242)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), h); err != nil {
		t.Fatal(err)
	}
	rec.cmds = nil
	e.SameContainer = func(fleet.Host) bool { return true }
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("cmds = %q", rec.cmds)
	}
}

func TestSameContainerComparesTheProcessCgroup(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "cgroup"), []byte("0::/system.slice/docker-abc.scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	same := SameContainer(proc)
	dir, err := cgroup.DirForPID(proc, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !same(fleet.Host{PID: 42, CgroupDir: dir}) {
		t.Error("the same process in the same cgroup reads as different")
	}
	if same(fleet.Host{PID: 42, CgroupDir: "/sys/fs/cgroup/system.slice/docker-other.scope"}) {
		t.Error("a reused PID in another cgroup reads as the same")
	}
	if same(fleet.Host{PID: 43, CgroupDir: dir}) {
		t.Error("a dead process reads as the same")
	}
}

// A hook that failed may have run anyway, for example a request that timed out. No
// state file is left behind, so nothing would ever undo it later.
func TestHookFaultRollsBackWhenItFails(t *testing.T) {
	for name, want := range map[string]string{
		"pool-leak":      "http://172.18.0.7:9090/admin/leak?n=0",
		"cpu-contention": "http://172.18.0.7:9090/admin/burn?threads=0",
	} {
		t.Run(name, func(t *testing.T) {
			e, rec := testEnv(t)
			rec.fail = func(call string) bool { return !strings.Contains(call, "=0") }
			h := testHost(t, "app-07", 1)
			if _, err := mustFault(t, name).Apply(context.Background(), e, h); err == nil {
				t.Fatal("want error")
			}
			if rec.posts[len(rec.posts)-1] != want {
				t.Fatalf("posts = %q, want a final %s", rec.posts, want)
			}
		})
	}
}
