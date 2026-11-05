package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// state is what a later process needs to undo a fault with nothing else to go on.
type state struct {
	Fault string            `json:"fault"`
	Host  fleet.Host        `json:"host"`
	Saved map[string]string `json:"saved"`
}

func statePath(e Env, host, faultName string) string {
	return filepath.Join(e.StateDir, host+"__"+faultName+".json")
}

// ApplyWithState applies f and records it. If the record cannot be written the
// fault is reverted, because nothing could undo it later.
func ApplyWithState(ctx context.Context, e Env, f Fault, h fleet.Host) error {
	if err := os.MkdirAll(e.StateDir, 0o755); err != nil {
		return err
	}
	path := statePath(e, h.Name, f.Name())
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s on %s: already applied", f.Name(), h.Name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	saved, err := f.Apply(ctx, e, h)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", f.Name(), h.Name, err)
	}
	if err := writeState(path, state{Fault: f.Name(), Host: h, Saved: saved}); err != nil {
		return fmt.Errorf("%s on %s: %w", f.Name(), h.Name, errors.Join(err, f.Revert(ctx, e, h, saved)))
	}
	return nil
}

// writeState renames into place so a crash cannot leave a partial state file.
func writeState(path string, s state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RevertAll reverts every recorded fault. It keeps going past errors, and a
// fault that failed to revert keeps its state file so it can be retried.
func RevertAll(ctx context.Context, e Env) error {
	paths, err := filepath.Glob(filepath.Join(e.StateDir, "*.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		errs = append(errs, revertFile(ctx, e, path))
	}
	return errors.Join(errs...)
}

// RevertOne reverts a single recorded fault.
func RevertOne(ctx context.Context, e Env, host, faultName string) error {
	path := statePath(e, host, faultName)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s on %s: not applied: %w", faultName, host, err)
	}
	return revertFile(ctx, e, path)
}

func revertFile(ctx context.Context, e Env, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// The container's cgroup goes away with it. Its faults went too, and its old
	// PID may belong to another process by now, so running the revert is unsafe.
	if _, err := os.Stat(s.Host.CgroupDir); errors.Is(err, fs.ErrNotExist) {
		return os.Remove(path)
	}

	f, ok := Registry()[s.Fault]
	if !ok {
		return fmt.Errorf("%s: unknown fault %q", path, s.Fault)
	}
	if err := f.Revert(ctx, e, s.Host, s.Saved); err != nil {
		return fmt.Errorf("%s on %s: %w", s.Fault, s.Host.Name, err)
	}
	return os.Remove(path)
}
