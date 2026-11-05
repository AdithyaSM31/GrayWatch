package chaos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// recorder stands in for the command runner and the admin hook client.
type recorder struct {
	cmds  []string
	posts []string
	fail  func(call string) bool
}

func (r *recorder) failed(call string) error {
	if r.fail != nil && r.fail(call) {
		return errors.New("failed: " + call)
	}
	return nil
}

func (r *recorder) Run(_ context.Context, name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	r.cmds = append(r.cmds, cmd)
	return r.failed(cmd)
}

func (r *recorder) post(_ context.Context, url string) error {
	r.posts = append(r.posts, url)
	return r.failed(url)
}

func testEnv(t *testing.T) (Env, *recorder) {
	t.Helper()
	rec := &recorder{}
	return Env{Run: rec, Post: rec.post, IODevice: "259:0", StateDir: t.TempDir()}, rec
}

// testHost returns a host whose cgroup directory is a temp dir holding the
// files the faults touch.
func testHost(t *testing.T, name string, pid int) fleet.Host {
	t.Helper()
	dir := t.TempDir()
	for file, content := range map[string]string{
		"cpu.max":        "max 100000\n",
		"io.max":         "",
		"memory.current": "104857600\n",
		"memory.high":    "max\n",
	} {
		writeFile(t, filepath.Join(dir, file), content)
	}
	resolv := filepath.Join(dir, "resolv.conf")
	writeFile(t, resolv, "nameserver 127.0.0.11\noptions ndots:0\n")
	return fleet.Host{Name: name, PID: pid, CgroupDir: dir, ResolvConf: resolv, IP: "172.18.0.7"}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustFault(t *testing.T, name string) Fault {
	t.Helper()
	f, ok := Registry()[name]
	if !ok {
		t.Fatalf("no fault named %q", name)
	}
	return f
}

func apply(t *testing.T, e Env, name string, h fleet.Host) map[string]string {
	t.Helper()
	saved, err := mustFault(t, name).Apply(context.Background(), e, h)
	if err != nil {
		t.Fatalf("Apply(%s): %v", name, err)
	}
	return saved
}

func revert(t *testing.T, e Env, name string, h fleet.Host, saved map[string]string) {
	t.Helper()
	if err := mustFault(t, name).Revert(context.Background(), e, h, saved); err != nil {
		t.Fatalf("Revert(%s): %v", name, err)
	}
}

func TestNetFaultCommands(t *testing.T) {
	const del = "nsenter -t 4242 -n tc qdisc del dev eth0 root"
	for name, want := range map[string]string{
		"net-loss":  "nsenter -t 4242 -n tc qdisc replace dev eth0 root netem loss 5%",
		"net-delay": "nsenter -t 4242 -n tc qdisc replace dev eth0 root netem delay 100ms 20ms",
		"dep-slow":  "nsenter -t 4242 -n tc qdisc replace dev eth0 root netem delay 300ms",
	} {
		e, rec := testEnv(t)
		h := testHost(t, "app-07", 4242)
		saved := apply(t, e, name, h)
		if !slices.Equal(rec.cmds, []string{want}) {
			t.Errorf("%s apply ran %q, want %q", name, rec.cmds, want)
		}
		rec.cmds = nil
		revert(t, e, name, h, saved)
		if !slices.Equal(rec.cmds, []string{del}) {
			t.Errorf("%s revert ran %q, want %q", name, rec.cmds, del)
		}
	}
}

func TestCPUThrottleSavesAndRestores(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	cpuMax := filepath.Join(h.CgroupDir, "cpu.max")

	saved := apply(t, e, "cpu-throttle", h)
	if got := readFile(t, cpuMax); got != "5000 100000" {
		t.Fatalf("after Apply cpu.max = %q", got)
	}
	revert(t, e, "cpu-throttle", h, saved)
	if got := readFile(t, cpuMax); got != "max 100000" {
		t.Fatalf("after Revert cpu.max = %q", got)
	}
}

func TestCPUContentionHooks(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	saved := apply(t, e, "cpu-contention", h)
	revert(t, e, "cpu-contention", h, saved)
	want := []string{"http://172.18.0.7:9090/admin/burn?threads=2", "http://172.18.0.7:9090/admin/burn?threads=0"}
	if !slices.Equal(rec.posts, want) {
		t.Fatalf("posts = %q, want %q", rec.posts, want)
	}
}

func TestPoolLeakHooks(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	saved := apply(t, e, "pool-leak", h)
	revert(t, e, "pool-leak", h, saved)
	want := []string{"http://172.18.0.7:9090/admin/leak?n=10", "http://172.18.0.7:9090/admin/leak?n=0"}
	if !slices.Equal(rec.posts, want) {
		t.Fatalf("posts = %q, want %q", rec.posts, want)
	}
}

func TestIOThrottleUsesDevice(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	ioMax := filepath.Join(h.CgroupDir, "io.max")

	saved := apply(t, e, "io-throttle", h)
	if got, want := readFile(t, ioMax), "259:0 rbps=1048576 wbps=1048576 riops=50 wiops=50"; got != want {
		t.Fatalf("after Apply io.max = %q, want %q", got, want)
	}

	// A later revert-all may not know the device, so it comes from the state.
	e.IODevice = ""
	revert(t, e, "io-throttle", h, saved)
	if got, want := readFile(t, ioMax), "259:0 rbps=max wbps=max riops=max wiops=max"; got != want {
		t.Fatalf("after Revert io.max = %q, want %q", got, want)
	}
}

func TestIOThrottleNeedsDevice(t *testing.T) {
	e, _ := testEnv(t)
	e.IODevice = ""
	if _, err := mustFault(t, "io-throttle").Apply(context.Background(), e, testHost(t, "app-07", 1)); err == nil {
		t.Fatal("want error")
	}
}

// io.max does not exist when the io controller is off. Creating it would make
// the fault look applied while throttling nothing.
func TestFaultsDoNotCreateMissingCgroupFiles(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	ioMax := filepath.Join(h.CgroupDir, "io.max")
	if err := os.Remove(ioMax); err != nil {
		t.Fatal(err)
	}
	if _, err := mustFault(t, "io-throttle").Apply(context.Background(), e, h); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(ioMax); err == nil {
		t.Fatal("io.max was created")
	}
}

func TestMemPressureSetsHighAboveCurrent(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	high := filepath.Join(h.CgroupDir, "memory.high")

	saved := apply(t, e, "mem-pressure", h)
	// memory.current is 100 MiB, so 164 MiB.
	if got := readFile(t, high); got != "171966464" {
		t.Fatalf("memory.high = %q, want 171966464", got)
	}
	if want := []string{"http://172.18.0.7:9090/admin/memhog?mb=300"}; !slices.Equal(rec.posts, want) {
		t.Fatalf("posts = %q, want %q", rec.posts, want)
	}

	revert(t, e, "mem-pressure", h, saved)
	if got := readFile(t, high); got != "max" {
		t.Fatalf("after Revert memory.high = %q, want max", got)
	}
	if rec.posts[len(rec.posts)-1] != "http://172.18.0.7:9090/admin/memhog?mb=0" {
		t.Fatalf("posts = %q, want memhog released last", rec.posts)
	}
}

// A failed Apply leaves no state file, so nothing would ever undo a half-applied
// fault later. It has to clean up after itself.
func TestMemPressureRollsBackWhenHookFails(t *testing.T) {
	e, rec := testEnv(t)
	rec.fail = func(call string) bool { return strings.Contains(call, "mb=300") }
	h := testHost(t, "app-07", 1)
	if _, err := mustFault(t, "mem-pressure").Apply(context.Background(), e, h); err == nil {
		t.Fatal("want error")
	}
	if got := readFile(t, filepath.Join(h.CgroupDir, "memory.high")); got != "max" {
		t.Fatalf("memory.high = %q, want it restored to max", got)
	}
	if rec.posts[len(rec.posts)-1] != "http://172.18.0.7:9090/admin/memhog?mb=0" {
		t.Fatalf("posts = %q, want a final release", rec.posts)
	}
}

// The memory hook can block while the kernel throttles the allocation, and only
// restoring memory.high lifts that, so a failed hook must not stop the restore.
func TestMemPressureRevertRestoresHighWhenHookFails(t *testing.T) {
	e, rec := testEnv(t)
	h := testHost(t, "app-07", 1)
	saved := apply(t, e, "mem-pressure", h)
	rec.fail = func(call string) bool { return strings.Contains(call, "mb=0") }
	if err := mustFault(t, "mem-pressure").Revert(context.Background(), e, h, saved); err == nil {
		t.Fatal("want the hook error reported")
	}
	if got := readFile(t, filepath.Join(h.CgroupDir, "memory.high")); got != "max" {
		t.Fatalf("memory.high = %q, want max", got)
	}
}

func TestDNSBreakRestoresOriginal(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	original := readFile(t, h.ResolvConf)

	saved := apply(t, e, "dns-break", h)
	if got := readFile(t, h.ResolvConf); got != "nameserver 192.0.2.1\n" {
		t.Fatalf("after Apply resolv.conf = %q", got)
	}
	revert(t, e, "dns-break", h, saved)
	if got := readFile(t, h.ResolvConf); got != original {
		t.Fatalf("after Revert resolv.conf = %q, want %q", got, original)
	}
}

// Docker bind-mounts resolv.conf by inode. Replacing the file with a rename
// would change it on the host while the container keeps seeing the old one.
func TestDNSBreakWritesInPlace(t *testing.T) {
	e, _ := testEnv(t)
	h := testHost(t, "app-07", 1)
	before, err := os.Stat(h.ResolvConf)
	if err != nil {
		t.Fatal(err)
	}
	saved := apply(t, e, "dns-break", h)
	revert(t, e, "dns-break", h, saved)
	after, err := os.Stat(h.ResolvConf)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("resolv.conf was replaced, not written in place")
	}
}

func TestRegistryHasNineFaults(t *testing.T) {
	want := map[string]string{
		"net-loss":       "network",
		"net-delay":      "network",
		"cpu-throttle":   "cpu",
		"cpu-contention": "cpu",
		"io-throttle":    "disk",
		"mem-pressure":   "memory",
		"dns-break":      "app",
		"pool-leak":      "app",
		"dep-slow":       "fleet",
	}
	got := Registry()
	if len(got) != len(want) {
		t.Fatalf("got %d faults, want %d", len(got), len(want))
	}
	for name, category := range want {
		f, ok := got[name]
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if f.Name() != name || f.Category() != category {
			t.Errorf("%s: Name/Category = %q/%q, want %q/%q", name, f.Name(), f.Category(), name, category)
		}
	}
}
