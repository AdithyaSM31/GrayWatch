// Package fleet finds the running containers and where their kernel files are.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
)

const appLabel = "graywatch.role=app"

type Host struct {
	Name       string
	PID        int
	CgroupDir  string
	ResolvConf string
	IP         string
}

type Docker interface {
	ListByLabel(ctx context.Context, label string) ([]string, error)
	Inspect(ctx context.Context, name string) (pid int, resolvConf, ip string, err error)
}

// Discover returns the app containers, sorted by name.
func Discover(ctx context.Context, d Docker, procRoot string) ([]Host, error) {
	names, err := d.ListByLabel(ctx, appLabel)
	if err != nil {
		return nil, err
	}
	hosts := make([]Host, 0, len(names))
	for _, name := range names {
		h, err := Lookup(ctx, d, procRoot, name)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	slices.SortFunc(hosts, func(a, b Host) int { return strings.Compare(a.Name, b.Name) })
	return hosts, nil
}

// Lookup resolves any running container by name, including dep.
func Lookup(ctx context.Context, d Docker, procRoot, name string) (Host, error) {
	pid, resolvConf, ip, err := d.Inspect(ctx, name)
	if err != nil {
		return Host{}, fmt.Errorf("%s: %w", name, err)
	}
	if pid <= 0 {
		return Host{}, fmt.Errorf("%s: not running", name)
	}
	dir, err := cgroup.DirForPID(procRoot, pid)
	if err != nil {
		return Host{}, fmt.Errorf("%s: %w", name, err)
	}
	return Host{Name: name, PID: pid, CgroupDir: dir, ResolvConf: resolvConf, IP: ip}, nil
}

// CLIDocker implements Docker with the docker command. The zero value is ready
// to use; run is only set by tests.
type CLIDocker struct {
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (d CLIDocker) output(ctx context.Context, args ...string) ([]byte, error) {
	run := d.run
	if run == nil {
		run = runCommand
	}
	return run(ctx, "docker", args...)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

func (d CLIDocker) ListByLabel(ctx context.Context, label string) ([]string, error) {
	out, err := d.output(ctx, "ps", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func (d CLIDocker) Inspect(ctx context.Context, name string) (int, string, string, error) {
	out, err := d.output(ctx, "inspect", "-f", "{{.State.Pid}} {{.ResolvConfPath}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
	if err != nil {
		return 0, "", "", err
	}
	// A stopped container has no IP, so its output has only two fields.
	fields := strings.Fields(string(out))
	if len(fields) != 2 && len(fields) != 3 {
		return 0, "", "", fmt.Errorf("unexpected docker inspect output %q", out)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", "", fmt.Errorf("unexpected docker inspect output %q", out)
	}
	ip := ""
	if len(fields) == 3 {
		ip = fields[2]
	}
	return pid, fields[1], ip, nil
}
