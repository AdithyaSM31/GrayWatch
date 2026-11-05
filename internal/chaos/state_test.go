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

func stateFiles(t *testing.T, e Env) []string {
	t.Helper()
	entries, err := os.ReadDir(e.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}

func TestApplyWritesStateFile(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "cpu-throttle"), h); err != nil {
		t.Fatal(err)
	}
	if files := stateFiles(t, e); len(files) != 1 || files[0] != "app-07__cpu-throttle.json" {
		t.Fatalf("state files = %q", files)
	}
}

func TestApplyCreatesStateDir(t *testing.T) {
	e, _ := testEnv(t)
	e.StateDir = filepath.Join(e.StateDir, "nested", "chaos")
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), testHost(t, "app-07", 1)); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTwiceFails(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	f := mustFault(t, "net-loss")
	if err := ApplyWithState(context.Background(), e, f, h); err != nil {
		t.Fatal(err)
	}
	err := ApplyWithState(context.Background(), e, f, h)
	if err == nil || !strings.Contains(err.Error(), "already applied") {
		t.Fatalf("err = %v, want already applied", err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("ran %d commands, want 1: the second apply must not touch the host", len(rec.cmds))
	}
}

func TestApplyFailureLeavesNoState(t *testing.T) {
	e, rec := testEnv(t)
	rec.fail = func(string) bool { return true }
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), testHost(t, "app-07", 1)); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q, want none", files)
	}
}

// stateLosingFault removes the state directory while applying, so the state
// file cannot be written afterwards.
type stateLosingFault struct {
	reverted *bool
}

func (stateLosingFault) Name() string     { return "state-losing" }
func (stateLosingFault) Category() string { return "test" }

func (stateLosingFault) Apply(_ context.Context, e Env, _ fleet.Host) (map[string]string, error) {
	return nil, os.RemoveAll(e.StateDir)
}

func (f stateLosingFault) Revert(context.Context, Env, fleet.Host, map[string]string) error {
	*f.reverted = true
	return nil
}

// A fault with no state file could never be reverted, so it must not stay applied.
func TestApplyRevertsWhenStateCannotBeWritten(t *testing.T) {
	e, _ := testEnv(t)
	reverted := false
	err := ApplyWithState(context.Background(), e, stateLosingFault{&reverted}, testHost(t, "app-07", 1))
	if err == nil {
		t.Fatal("want error")
	}
	if !reverted {
		t.Fatal("fault was left applied")
	}
}

func TestRevertAllUsesStateFiles(t *testing.T) {
	e, _ := testEnv(t)
	throttled := testHost(t, "app-03", 3)
	lossy := testHost(t, "app-05", 5)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "cpu-throttle"), throttled); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), lossy); err != nil {
		t.Fatal(err)
	}

	// A new process knows nothing except the state directory.
	rec := &recorder{}
	fresh := Env{Run: rec, Post: rec.post, StateDir: e.StateDir}
	if err := RevertAll(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(throttled.CgroupDir, "cpu.max")); got != "max 100000" {
		t.Errorf("cpu.max = %q, want it restored", got)
	}
	if want := "nsenter -t 5 -n tc qdisc del dev eth0 root"; len(rec.cmds) != 1 || rec.cmds[0] != want {
		t.Errorf("cmds = %q, want %q", rec.cmds, want)
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Errorf("state files = %q, want none", files)
	}
}

func TestRevertAllContinuesOnError(t *testing.T) {
	e, rec := testEnv(t)
	first, second := testHost(t, "app-01", 1), testHost(t, "app-02", 2)
	for _, h := range []fleet.Host{first, second} {
		if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), h); err != nil {
			t.Fatal(err)
		}
	}
	rec.fail = func(call string) bool { return strings.HasPrefix(call, "nsenter -t 1 ") }

	err := RevertAll(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "app-01") {
		t.Fatalf("err = %v, want one naming app-01", err)
	}
	// The failed one keeps its state file so it can be retried.
	if files := stateFiles(t, e); len(files) != 1 || files[0] != "app-01__net-loss.json" {
		t.Fatalf("state files = %q, want only app-01's", files)
	}
	if !strings.HasPrefix(rec.cmds[len(rec.cmds)-1], "nsenter -t 2 ") {
		t.Fatalf("cmds = %q, want app-02 attempted after the failure", rec.cmds)
	}
}

func TestRevertAllRetrySucceeds(t *testing.T) {
	e, rec := testEnv(t)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), testHost(t, "app-01", 1)); err != nil {
		t.Fatal(err)
	}
	rec.fail = func(string) bool { return true }
	if err := RevertAll(context.Background(), e); err == nil {
		t.Fatal("want error")
	}
	rec.fail = nil
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q, want none", files)
	}
}

type runnerFunc func(cmd string) error

func (f runnerFunc) Run(_ context.Context, name string, args ...string) error {
	return f(name + " " + strings.Join(args, " "))
}

// A crash between reverting and deleting the state file leaves a record of a
// qdisc that is already gone, and tc refuses to delete it. Treating that as a
// failure would mean the retry never succeeds and the next apply is refused.
// The message is what tc prints on a real kernel when there is nothing to delete.
func TestRevertAllSucceedsWhenQdiscAlreadyGone(t *testing.T) {
	e, _ := testEnv(t)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), testHost(t, "app-01", 1)); err != nil {
		t.Fatal(err)
	}
	e.Run = runnerFunc(func(string) error {
		return errors.New("exit status 2: Error: Cannot delete qdisc with handle of zero.")
	})
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q, want none", files)
	}
}

func TestNetRevertStillReportsOtherErrors(t *testing.T) {
	e, _ := testEnv(t)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), testHost(t, "app-01", 1)); err != nil {
		t.Fatal(err)
	}
	e.Run = runnerFunc(func(string) error { return errors.New("nsenter: cannot open /proc/1/ns/net") })
	if err := RevertAll(context.Background(), e); err == nil {
		t.Fatal("want error")
	}
	if files := stateFiles(t, e); len(files) != 1 {
		t.Fatalf("state files = %q, want it kept for a retry", files)
	}
}

// When a container is gone its faults went with it, and its old PID may belong
// to something else by now. Running the revert could hit the wrong process, and
// keeping the file would make the next apply fail with "already applied".
func TestRevertAllDropsStateWhenContainerGone(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-01", 1)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "net-loss"), h); err != nil {
		t.Fatal(err)
	}
	rec.cmds = nil
	if err := os.RemoveAll(h.CgroupDir); err != nil {
		t.Fatal(err)
	}
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("ran %q against a container that is gone", rec.cmds)
	}
	if files := stateFiles(t, e); len(files) != 0 {
		t.Fatalf("state files = %q, want none", files)
	}
}

func TestRevertAllSkipsBadFilesAndKeepsGoing(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-02", 2)
	if err := ApplyWithState(context.Background(), e, mustFault(t, "cpu-throttle"), h); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.StateDir, "app-01__corrupt.json"), "{not json")
	writeFile(t, filepath.Join(e.StateDir, "app-01__unknown.json"), `{"fault":"no-such-fault","host":{"Name":"app-01","CgroupDir":"`+filepath.ToSlash(h.CgroupDir)+`"}}`)

	if err := RevertAll(context.Background(), e); err == nil {
		t.Fatal("want errors for the bad files")
	}
	if got := readFile(t, filepath.Join(h.CgroupDir, "cpu.max")); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want the good entry reverted", got)
	}
	if files := stateFiles(t, e); len(files) != 2 {
		t.Fatalf("state files = %q, want the two bad ones kept", files)
	}
}

func TestRevertAllWithNoStateDir(t *testing.T) {
	e, _ := testEnv(t)
	e.StateDir = filepath.Join(e.StateDir, "never-created")
	if err := RevertAll(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestRevertOne(t *testing.T) {
	e, _ := testEnv(t)
	a, b := testHost(t, "app-01", 1), testHost(t, "app-02", 2)
	for _, h := range []fleet.Host{a, b} {
		if err := ApplyWithState(context.Background(), e, mustFault(t, "cpu-throttle"), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := RevertOne(context.Background(), e, "app-01", "cpu-throttle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(a.CgroupDir, "cpu.max")); got != "max 100000" {
		t.Errorf("app-01 cpu.max = %q, want it restored", got)
	}
	if got := readFile(t, filepath.Join(b.CgroupDir, "cpu.max")); got != "5000 100000" {
		t.Errorf("app-02 cpu.max = %q, want it untouched", got)
	}
}

func TestRevertOneNotApplied(t *testing.T) {
	e, _ := testEnv(t)
	err := RevertOne(context.Background(), e, "app-01", "cpu-throttle")
	if err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("err = %v, want not applied", err)
	}
	if errors.Is(err, os.ErrNotExist) == false {
		t.Fatalf("err = %v, want it to wrap os.ErrNotExist", err)
	}
}
