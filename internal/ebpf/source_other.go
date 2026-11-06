//go:build !linux

package ebpf

import "errors"

var errNotLinux = errors.New("eBPF signals need Linux")

// Source is not available on this platform.
type Source struct{}

// Load always fails on this platform.
func Load() (*Source, error) { return nil, errNotLinux }

func (*Source) P99ByCgroup() (map[uint64]float64, error) { return nil, errNotLinux }

func (*Source) Close() error { return nil }

// CgroupID always fails on this platform.
func CgroupID(string) (uint64, error) { return 0, errNotLinux }
