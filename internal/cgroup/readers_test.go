package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPSI(t *testing.T) {
	path := writeTemp(t, "some avg10=1.23 avg60=0.50 avg300=0.10 total=123456\nfull avg10=0.10 avg60=0.05 avg300=0.01 total=7890\n")
	got, err := ReadPSI(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SomeTotal != 123456*time.Microsecond || got.FullTotal != 7890*time.Microsecond {
		t.Fatalf("got %+v, want some 123456µs, full 7890µs", got)
	}
}

func TestReadPSISomeOnly(t *testing.T) {
	got, err := ReadPSI(writeTemp(t, "some avg10=0.00 avg60=0.00 avg300=0.00 total=42\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.SomeTotal != 42*time.Microsecond || got.FullTotal != 0 {
		t.Fatalf("got %+v, want some 42µs, full 0", got)
	}
}

func TestReadPSIRealFile(t *testing.T) {
	got, err := ReadPSI("testdata/wsl_cpu.pressure")
	if err != nil {
		t.Fatal(err)
	}
	if got.SomeTotal != 109676*time.Microsecond || got.FullTotal != 0 {
		t.Fatalf("got %+v, want some 109676µs, full 0", got)
	}
}

func TestReadPSIRejectsBadInput(t *testing.T) {
	for name, content := range map[string]string{
		"no some line": "full avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"no total":     "some avg10=0.00 avg60=0.00 avg300=0.00\n",
		"bad total":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=abc\n",
		"empty":        "",
	} {
		if _, err := ReadPSI(writeTemp(t, content)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ReadPSI(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: want error")
	}
}

func TestReadCPUStat(t *testing.T) {
	got, err := ReadCPUStat(writeTemp(t, "usage_usec 9000\nuser_usec 6000\nsystem_usec 3000\nnr_periods 50\nnr_throttled 5\nthrottled_usec 2500\nnr_bursts 0\nburst_usec 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := CPUStat{UsageUsec: 9000, NrPeriods: 50, NrThrottled: 5, ThrottledUsec: 2500}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadCPUStatRealFile(t *testing.T) {
	got, err := ReadCPUStat("testdata/wsl_cpu.stat")
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageUsec != 4660000 {
		t.Fatalf("UsageUsec = %d, want 4660000", got.UsageUsec)
	}
}

// Without the cpu controller the kernel writes only the usage lines.
func TestReadCPUStatWithoutThrottleLines(t *testing.T) {
	got, err := ReadCPUStat(writeTemp(t, "usage_usec 9000\nuser_usec 6000\nsystem_usec 3000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != (CPUStat{UsageUsec: 9000}) {
		t.Fatalf("got %+v", got)
	}
}

func TestReadCPUStatRejectsBadValue(t *testing.T) {
	if _, err := ReadCPUStat(writeTemp(t, "throttled_usec abc\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestReadUint(t *testing.T) {
	got, err := ReadUint(writeTemp(t, "104857600\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 104857600 {
		t.Fatalf("got %d", got)
	}
	if _, err := ReadUint(writeTemp(t, "max\n")); err == nil {
		t.Fatal("non-numeric value: want error")
	}
}

func writeProcCgroup(t *testing.T, pid, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, pid, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDirForPID(t *testing.T) {
	root := writeProcCgroup(t, "4242", "0::/system.slice/docker-abc.scope\n")
	got, err := DirForPID(root, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/sys/fs/cgroup/system.slice/docker-abc.scope"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDirForPIDRootCgroup(t *testing.T) {
	got, err := DirForPID(writeProcCgroup(t, "1", "0::/\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/sys/fs/cgroup" {
		t.Fatalf("got %q", got)
	}
}

func TestDirForPIDCgroupV1(t *testing.T) {
	_, err := DirForPID(writeProcCgroup(t, "1", "4:memory:/docker/abc\n"), 1)
	if err == nil || !strings.Contains(err.Error(), "cgroup v1") {
		t.Fatalf("err = %v, want one mentioning cgroup v1", err)
	}
}

func TestDirForPIDMissingProcess(t *testing.T) {
	if _, err := DirForPID(t.TempDir(), 99); err == nil {
		t.Fatal("want error")
	}
}
