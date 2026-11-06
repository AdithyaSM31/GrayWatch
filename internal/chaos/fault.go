// Package chaos injects real, reversible faults into containers and records
// enough on disk to undo them after a crash.
package chaos

import (
	"context"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// Runner runs a host command, such as nsenter.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type Env struct {
	Run      Runner
	Post     func(ctx context.Context, url string) error
	IODevice string // MAJ:MIN of the whole disk, needed to apply io-throttle
	StateDir string

	// SameContainer reports whether a recorded host is still the same running
	// container. Nil assumes it is. See SameContainer in state.go.
	SameContainer func(fleet.Host) bool
}

// Snapshotter is a fault that can say what its revert will need before it changes
// anything, so a crash partway through applying it can still be undone.
type Snapshotter interface {
	Snapshot(e Env, h fleet.Host) (map[string]string, error)
}

// Fault is one kind of failure. Apply returns whatever Revert will need, and
// must not leave the fault half-applied when it returns an error.
type Fault interface {
	Name() string
	Category() string
	Apply(ctx context.Context, e Env, h fleet.Host) (saved map[string]string, err error)
	Revert(ctx context.Context, e Env, h fleet.Host, saved map[string]string) error
}
