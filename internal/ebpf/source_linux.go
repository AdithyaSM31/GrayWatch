//go:build linux

package ebpf

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// Regenerating needs clang, libbpf-dev and a vmlinux.h beside the .c file, which
// comes from bpftool. The compiled objects are committed, so building doesn't.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -tags linux -type hist runqlat runqlat.bpf.c -- -I. -O2 -g

// Source reads run queue latency from a loaded BPF program.
type Source struct {
	objs  runqlatObjects
	links []link.Link
	prev  map[uint64][Slots]uint64
}

// Load loads the program and attaches it to the scheduler. It needs root and a
// kernel with BTF. Close it when done.
func Load() (*Source, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("removing the memlock limit: %w", err)
	}
	s := &Source{prev: map[uint64][Slots]uint64{}}
	if err := loadRunqlatObjects(&s.objs, nil); err != nil {
		return nil, fmt.Errorf("loading the BPF program: %w", err)
	}
	for _, prog := range []struct {
		name   string
		attach func() (link.Link, error)
	}{
		{"sched_wakeup", func() (link.Link, error) { return link.AttachTracing(link.TracingOptions{Program: s.objs.OnWakeup}) }},
		{"sched_wakeup_new", func() (link.Link, error) { return link.AttachTracing(link.TracingOptions{Program: s.objs.OnWakeupNew}) }},
		{"sched_switch", func() (link.Link, error) { return link.AttachTracing(link.TracingOptions{Program: s.objs.OnSwitch}) }},
	} {
		l, err := prog.attach()
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("attaching to %s: %w", prog.name, err)
		}
		s.links = append(s.links, l)
	}
	return s, nil
}

// P99ByCgroup returns each cgroup's 99th percentile since the last call.
func (s *Source) P99ByCgroup() (map[uint64]float64, error) {
	out := map[uint64]float64{}
	var (
		id   uint64
		hist runqlatHist
	)
	iter := s.objs.Hists.Iterate()
	for iter.Next(&id, &hist) {
		cur := [Slots]uint64(hist.Slots)
		if p99, ok := P99(Delta(cur, s.prev[id])); ok {
			out[id] = p99
		}
		s.prev[id] = cur
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("reading histograms: %w", err)
	}
	return out, nil
}

// Close detaches the program.
func (s *Source) Close() error {
	var errs []error
	for _, l := range s.links {
		errs = append(errs, l.Close())
	}
	errs = append(errs, s.objs.Close())
	return errors.Join(errs...)
}

// CgroupID returns a cgroup directory's id, which on cgroup v2 is its inode number.
func CgroupID(dir string) (uint64, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no inode number", dir)
	}
	return stat.Ino, nil
}
