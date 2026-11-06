package chaos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// A hook that failed may have run anyway. If its undo failed too, the fault may be
// live, and the record is the only way to find it again.
func TestFailedUndoKeepsTheRecord(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	rec.fail = func(string) bool { return true }
	if err := ApplyWithState(context.Background(), e, mustFault(t, "pool-leak"), h); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 1 {
		t.Fatalf("state files = %q, want the record kept", files)
	}
	rec.fail = nil
	if err := RevertOne(context.Background(), e, h.Name, "pool-leak"); err != nil {
		t.Fatal(err)
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q after the revert", files)
	}
}

func TestUndoneHookLeavesNoRecord(t *testing.T) {
	e, rec := testEnv(t)
	rec.fail = func(call string) bool { return strings.Contains(call, "leak?n=10") }
	if err := ApplyWithState(context.Background(), e, mustFault(t, "pool-leak"), testHost(t, "app-07", 1)); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q", files)
	}
}

// mem-pressure's rollback is two steps, and the second can fail on its own.
func TestFailedMemPressureUndoKeepsTheRecord(t *testing.T) {
	e, rec := testEnv(t)
	rec.fail = func(call string) bool { return strings.Contains(call, "memhog") }
	h := testHost(t, "app-07", 1)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "mem-pressure"), h); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 1 {
		t.Fatalf("state files = %q, want the record kept: the hog may be running", files)
	}
}

// Writing resolv.conf truncates it first, so a failed write can leave it empty. The
// record holds the only copy of what it said.
func TestFailedDNSWriteThatCannotBeUndoneKeepsTheRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	if err := os.Chmod(h.ResolvConf, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWithState(context.Background(), e, mustFault(t, "dns-break"), h); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 1 {
		t.Fatalf("state files = %q, want the record kept", files)
	}
}

// tmpBlockingFault stops the final record being written, while the first one stays.
type tmpBlockingFault struct {
	revert error
	done   *bool
}

func (tmpBlockingFault) Name() string     { return "tmp-blocking" }
func (tmpBlockingFault) Category() string { return "test" }

func (tmpBlockingFault) Apply(_ context.Context, e Env, h fleet.Host) (map[string]string, error) {
	return nil, os.Mkdir(statePath(e, h.Name, "tmp-blocking")+".tmp", 0o755)
}

func (f tmpBlockingFault) Revert(context.Context, Env, fleet.Host, map[string]string) error {
	*f.done = true
	return f.revert
}

func jsonFiles(t *testing.T, e Env) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(e.StateDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRevertAfterALostRecordRemovesTheFirstOne(t *testing.T) {
	e, _ := testEnv(t)
	done := false
	f := tmpBlockingFault{done: &done}
	if err := ApplyWithState(context.Background(), e, f, testHost(t, "app-07", 1)); err == nil {
		t.Fatal("want error")
	}
	if !done {
		t.Fatal("not reverted")
	}
	if files := jsonFiles(t, e); len(files) != 0 {
		t.Fatalf("records = %q", files)
	}
}

func TestFailedRevertAfterALostRecordKeepsTheFirstOne(t *testing.T) {
	e, _ := testEnv(t)
	done := false
	f := tmpBlockingFault{revert: errors.New("cgroup busy"), done: &done}
	if err := ApplyWithState(context.Background(), e, f, testHost(t, "app-07", 1)); err == nil {
		t.Fatal("want error")
	}
	if files := jsonFiles(t, e); len(files) != 1 {
		t.Fatalf("records = %q, want the record kept: the fault is still on", files)
	}
}

// Only a process that is gone proves a restart. Any other read error says nothing, and
// dropping the record on it would lose track of a live fault.
func TestSameContainerKeepsTheRecordWhenItCannotTell(t *testing.T) {
	proc := t.TempDir()
	// Reading a directory fails with something other than "not found".
	if err := os.MkdirAll(filepath.Join(proc, "42", "cgroup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !SameContainer(proc)(fleet.Host{PID: 42, CgroupDir: "/sys/fs/cgroup/x"}) {
		t.Error("a failed read reads as a restart")
	}
}

func TestLeftoverRecordSaysHowToClearIt(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	f := mustFault(t, "net-loss")
	if err := ApplyWithState(context.Background(), e, f, h); err != nil {
		t.Fatal(err)
	}
	err := ApplyWithState(context.Background(), e, f, h)
	if err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("err = %v, want it to say to revert", err)
	}
}
