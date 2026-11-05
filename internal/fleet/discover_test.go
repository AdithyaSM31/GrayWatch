package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type containerInfo struct {
	pid            int
	resolvConf, ip string
	err            error
}

type fakeDocker struct {
	names      []string
	listErr    error
	containers map[string]containerInfo
	gotLabel   string
}

func (d *fakeDocker) ListByLabel(_ context.Context, label string) ([]string, error) {
	d.gotLabel = label
	return d.names, d.listErr
}

func (d *fakeDocker) Inspect(_ context.Context, name string) (int, string, string, error) {
	c := d.containers[name]
	return c.pid, c.resolvConf, c.ip, c.err
}

// fakeProc writes <root>/<pid>/cgroup for each pid.
func fakeProc(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, pid := range pids {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("0::/system.slice/docker-%d.scope\n", pid)
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDiscoverSortsAndFills(t *testing.T) {
	d := &fakeDocker{
		names: []string{"app-02", "app-01"},
		containers: map[string]containerInfo{
			"app-01": {pid: 101, resolvConf: "/rc/1", ip: "172.18.0.1"},
			"app-02": {pid: 102, resolvConf: "/rc/2", ip: "172.18.0.2"},
		},
	}
	got, err := Discover(context.Background(), d, fakeProc(t, 101, 102))
	if err != nil {
		t.Fatal(err)
	}
	want := []Host{
		{Name: "app-01", PID: 101, CgroupDir: "/sys/fs/cgroup/system.slice/docker-101.scope", ResolvConf: "/rc/1", IP: "172.18.0.1"},
		{Name: "app-02", PID: 102, CgroupDir: "/sys/fs/cgroup/system.slice/docker-102.scope", ResolvConf: "/rc/2", IP: "172.18.0.2"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if d.gotLabel != "graywatch.role=app" {
		t.Errorf("label = %q, want graywatch.role=app", d.gotLabel)
	}
}

func TestDiscoverInspectError(t *testing.T) {
	d := &fakeDocker{
		names: []string{"app-01", "app-02"},
		containers: map[string]containerInfo{
			"app-01": {pid: 101, resolvConf: "/rc/1", ip: "172.18.0.1"},
			"app-02": {err: errors.New("boom")},
		},
	}
	_, err := Discover(context.Background(), d, fakeProc(t, 101))
	if err == nil || !strings.Contains(err.Error(), "app-02") {
		t.Fatalf("err = %v, want one naming app-02", err)
	}
}

func TestDiscoverListError(t *testing.T) {
	d := &fakeDocker{listErr: errors.New("daemon down")}
	if _, err := Discover(context.Background(), d, t.TempDir()); err == nil {
		t.Fatal("want error")
	}
}

func TestLookupResolvesAnyContainer(t *testing.T) {
	d := &fakeDocker{containers: map[string]containerInfo{
		"dep": {pid: 55, resolvConf: "/rc/dep", ip: "172.18.0.9"},
	}}
	got, err := Lookup(context.Background(), d, fakeProc(t, 55), "dep")
	if err != nil {
		t.Fatal(err)
	}
	want := Host{Name: "dep", PID: 55, CgroupDir: "/sys/fs/cgroup/system.slice/docker-55.scope", ResolvConf: "/rc/dep", IP: "172.18.0.9"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// docker inspect reports PID 0 for a stopped container, and chaos would
// otherwise aim nsenter at the wrong process.
func TestLookupRejectsStoppedContainer(t *testing.T) {
	d := &fakeDocker{containers: map[string]containerInfo{"app-01": {pid: 0, resolvConf: "/rc", ip: ""}}}
	_, err := Lookup(context.Background(), d, fakeProc(t), "app-01")
	if err == nil || !strings.Contains(err.Error(), "app-01") {
		t.Fatalf("err = %v, want one naming app-01", err)
	}
}

// fakeRun stands in for the docker binary and records its arguments.
func fakeRun(output string, err error, got *[]string) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*got = append([]string{name}, args...)
		return []byte(output), err
	}
}

func TestCLIDockerListByLabel(t *testing.T) {
	var args []string
	d := CLIDocker{run: fakeRun("app-02\napp-01\n", nil, &args)}
	got, err := d.ListByLabel(context.Background(), "graywatch.role=app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"app-02", "app-01"}) {
		t.Fatalf("got %v", got)
	}
	want := []string{"docker", "ps", "--filter", "label=graywatch.role=app", "--format", "{{.Names}}"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestCLIDockerListByLabelEmpty(t *testing.T) {
	var args []string
	got, err := CLIDocker{run: fakeRun("\n", nil, &args)}.ListByLabel(context.Background(), "x=y")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want none", got)
	}
}

func TestCLIDockerInspect(t *testing.T) {
	var args []string
	d := CLIDocker{run: fakeRun("4242 /var/lib/docker/containers/abc/resolv.conf 172.18.0.5\n", nil, &args)}
	pid, resolv, ip, err := d.Inspect(context.Background(), "app-07")
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4242 || resolv != "/var/lib/docker/containers/abc/resolv.conf" || ip != "172.18.0.5" {
		t.Fatalf("got %d %q %q", pid, resolv, ip)
	}
	want := []string{"docker", "inspect", "-f", "{{.State.Pid}} {{.ResolvConfPath}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "app-07"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestCLIDockerInspectStoppedContainerHasNoIP(t *testing.T) {
	var args []string
	pid, _, ip, err := CLIDocker{run: fakeRun("0 /rc \n", nil, &args)}.Inspect(context.Background(), "app-07")
	if err != nil {
		t.Fatal(err)
	}
	if pid != 0 || ip != "" {
		t.Fatalf("got pid %d, ip %q", pid, ip)
	}
}

func TestCLIDockerInspectRejectsBadOutput(t *testing.T) {
	for _, output := range []string{"", "abc /rc 172.18.0.5\n", "1\n", "1 /rc 172.18.0.5 extra\n"} {
		var args []string
		if _, _, _, err := (CLIDocker{run: fakeRun(output, nil, &args)}).Inspect(context.Background(), "app-07"); err == nil {
			t.Errorf("output %q: want error", output)
		}
	}
}

func TestCLIDockerPassesCommandErrors(t *testing.T) {
	var args []string
	d := CLIDocker{run: fakeRun("", errors.New("no such container"), &args)}
	if _, err := d.ListByLabel(context.Background(), "x=y"); err == nil {
		t.Error("ListByLabel: want error")
	}
	if _, _, _, err := d.Inspect(context.Background(), "app-07"); err == nil {
		t.Error("Inspect: want error")
	}
}
