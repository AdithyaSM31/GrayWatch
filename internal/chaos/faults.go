package chaos

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

const (
	adminPort = "9090"

	// memHighHeadroom is how far above the service's anonymous memory
	// memory.high goes. Page cache is left out on purpose: a host caches its log at
	// about 430 MB an hour, and the kernel drops that for free, so counting it
	// made the fault weaker the longer a host had run, until it did nothing.
	memHighHeadroom = 64 << 20
	memHogMB        = 300

	// poolLeakConns takes the whole pool. Holding 9 of 10 caused no failures in
	// testing: each query needs a connection for about 1 ms, so one spare is
	// plenty at this request rate.
	poolLeakConns = 10

	// noQdiscToDelete is what tc prints when the qdisc is already gone. A revert
	// retried after a crash must count that as done, or it could never finish.
	noQdiscToDelete = "Cannot delete qdisc with handle of zero"
)

type applyFunc func(ctx context.Context, e Env, h fleet.Host) (map[string]string, error)
type revertFunc func(ctx context.Context, e Env, h fleet.Host, saved map[string]string) error

type fault struct {
	name, category string
	apply          applyFunc
	revert         revertFunc
	snapshot       func(e Env, h fleet.Host) (map[string]string, error) // nil if revert needs nothing
}

func (f fault) Name() string     { return f.name }
func (f fault) Category() string { return f.category }

func (f fault) Snapshot(e Env, h fleet.Host) (map[string]string, error) {
	if f.snapshot == nil {
		return nil, nil
	}
	return f.snapshot(e, h)
}

func (f fault) Apply(ctx context.Context, e Env, h fleet.Host) (map[string]string, error) {
	return f.apply(ctx, e, h)
}

func (f fault) Revert(ctx context.Context, e Env, h fleet.Host, saved map[string]string) error {
	return f.revert(ctx, e, h, saved)
}

// Registry returns every fault by name.
func Registry() map[string]Fault {
	faults := []fault{
		netem("net-loss", "network", "loss", "5%"),
		netem("net-delay", "network", "delay", "100ms", "20ms"),
		netem("dep-slow", "fleet", "delay", "300ms"),
		saveAndWrite("cpu-throttle", "cpu", "cpu.max", "5000 100000"),
		hook("cpu-contention", "cpu", "burn?threads=2", "burn?threads=0"),
		hook("pool-leak", "app", fmt.Sprintf("leak?n=%d", poolLeakConns), "leak?n=0"),
		{"io-throttle", "disk", applyIOThrottle, revertIOThrottle, snapshotIOThrottle},
		{"mem-pressure", "memory", applyMemPressure, revertMemPressure, snapshotMemPressure},
		{"dns-break", "app", applyDNSBreak, revertDNSBreak, snapshotDNSBreak},
	}
	registry := make(map[string]Fault, len(faults))
	for _, f := range faults {
		registry[f.name] = f
	}
	return registry
}

// netem changes the container's network from the host: nsenter enters only its
// network namespace, so the host's tc binary runs and the image needs no tc.
func netem(name, category string, args ...string) fault {
	tc := func(ctx context.Context, e Env, h fleet.Host, args ...string) error {
		base := []string{"-t", strconv.Itoa(h.PID), "-n", "tc", "qdisc"}
		return e.Run.Run(ctx, "nsenter", append(base, args...)...)
	}
	return fault{
		name:     name,
		category: category,
		apply: func(ctx context.Context, e Env, h fleet.Host) (map[string]string, error) {
			return nil, tc(ctx, e, h, append([]string{"replace", "dev", "eth0", "root", "netem"}, args...)...)
		},
		revert: func(ctx context.Context, e Env, h fleet.Host, _ map[string]string) error {
			err := tc(ctx, e, h, "del", "dev", "eth0", "root")
			if err != nil && strings.Contains(err.Error(), noQdiscToDelete) {
				return nil
			}
			return err
		},
	}
}

func saveAndWrite(name, category, file, value string) fault {
	snapshot := func(_ Env, h fleet.Host) (map[string]string, error) {
		old, err := readTrimmed(filepath.Join(h.CgroupDir, file))
		if err != nil {
			return nil, err
		}
		return map[string]string{file: old}, nil
	}
	return fault{
		name:     name,
		category: category,
		snapshot: snapshot,
		apply: func(_ context.Context, e Env, h fleet.Host) (map[string]string, error) {
			saved, err := snapshot(e, h)
			if err != nil {
				return nil, err
			}
			return saved, writeExisting(filepath.Join(h.CgroupDir, file), value)
		},
		revert: func(_ context.Context, _ Env, h fleet.Host, saved map[string]string) error {
			return writeExisting(filepath.Join(h.CgroupDir, file), saved[file])
		},
	}
}

func hook(name, category, applyPath, revertPath string) fault {
	return fault{
		name:     name,
		category: category,
		apply: func(ctx context.Context, e Env, h fleet.Host) (map[string]string, error) {
			err := e.Post(ctx, adminURL(h, applyPath))
			if err != nil {
				// It may have run anyway, such as a request that timed out, or partly
				// run, such as a pool that gave up some of its connections.
				return nil, errors.Join(err, e.Post(context.WithoutCancel(ctx), adminURL(h, revertPath)))
			}
			return nil, nil
		},
		revert: func(ctx context.Context, e Env, h fleet.Host, _ map[string]string) error {
			return e.Post(ctx, adminURL(h, revertPath))
		},
	}
}

func snapshotIOThrottle(e Env, _ fleet.Host) (map[string]string, error) {
	return map[string]string{"device": e.IODevice}, nil
}

func snapshotMemPressure(_ Env, h fleet.Host) (map[string]string, error) {
	old, err := readTrimmed(filepath.Join(h.CgroupDir, "memory.high"))
	if err != nil {
		return nil, err
	}
	return map[string]string{"memory.high": old}, nil
}

func snapshotDNSBreak(_ Env, h fleet.Host) (map[string]string, error) {
	old, err := os.ReadFile(h.ResolvConf)
	if err != nil {
		return nil, err
	}
	return map[string]string{"resolv.conf": string(old)}, nil
}

func applyIOThrottle(_ context.Context, e Env, h fleet.Host) (map[string]string, error) {
	if e.IODevice == "" {
		return nil, errors.New("io-throttle needs the disk device (MAJ:MIN)")
	}
	// A host writes 30 times a second at 600 rps across 20 hosts, one synced 4 KiB
	// write per request. 50 never bites. Measured on ext4: 25 and below stall the
	// host, and 10 and below also slow its peers enough to raise fleet alerts.
	limit := e.IODevice + " rbps=1048576 wbps=1048576 riops=50 wiops=20"
	return map[string]string{"device": e.IODevice}, writeExisting(filepath.Join(h.CgroupDir, "io.max"), limit)
}

func revertIOThrottle(_ context.Context, _ Env, h fleet.Host, saved map[string]string) error {
	limit := saved["device"] + " rbps=max wbps=max riops=max wiops=max"
	return writeExisting(filepath.Join(h.CgroupDir, "io.max"), limit)
}

func applyMemPressure(ctx context.Context, e Env, h fleet.Host) (map[string]string, error) {
	high := filepath.Join(h.CgroupDir, "memory.high")
	old, err := readTrimmed(high)
	if err != nil {
		return nil, err
	}
	anon, err := cgroup.ReadMemAnon(filepath.Join(h.CgroupDir, "memory.stat"))
	if err != nil {
		return nil, err
	}
	if err := writeExisting(high, strconv.FormatUint(anon+memHighHeadroom, 10)); err != nil {
		return nil, err
	}
	if err := e.Post(ctx, adminURL(h, fmt.Sprintf("memhog?mb=%d", memHogMB))); err != nil {
		// An interrupt during the hook must not leave the limit on and the hog running.
		return nil, errors.Join(err, restoreMemHigh(context.WithoutCancel(ctx), e, h, old))
	}
	return map[string]string{"memory.high": old}, nil
}

func revertMemPressure(ctx context.Context, e Env, h fleet.Host, saved map[string]string) error {
	return restoreMemHigh(ctx, e, h, saved["memory.high"])
}

// restoreMemHigh writes memory.high before releasing the memory hog. The hog's
// allocation is stalled in kernel throttling until the limit goes up.
func restoreMemHigh(ctx context.Context, e Env, h fleet.Host, old string) error {
	return errors.Join(
		writeExisting(filepath.Join(h.CgroupDir, "memory.high"), old),
		e.Post(ctx, adminURL(h, "memhog?mb=0")),
	)
}

func applyDNSBreak(_ context.Context, _ Env, h fleet.Host) (map[string]string, error) {
	old, err := os.ReadFile(h.ResolvConf)
	if err != nil {
		return nil, err
	}
	return map[string]string{"resolv.conf": string(old)}, writeExisting(h.ResolvConf, "nameserver 192.0.2.1\n")
}

func revertDNSBreak(_ context.Context, _ Env, h fleet.Host, saved map[string]string) error {
	return writeExisting(h.ResolvConf, saved["resolv.conf"])
}

func adminURL(h fleet.Host, path string) string {
	return "http://" + net.JoinHostPort(h.IP, adminPort) + "/admin/" + path
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeExisting overwrites a file in place and never creates it. A missing
// cgroup file means a controller is off, which must be an error. Keeping the
// same inode matters for resolv.conf, which Docker bind-mounts by inode.
func writeExisting(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return errors.Join(err, f.Close())
}
