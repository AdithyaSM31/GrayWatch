package cgroup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type CPUStat struct {
	UsageUsec, NrPeriods, NrThrottled, ThrottledUsec uint64
}

// ReadCPUStat parses cpu.stat. Lines it does not need are ignored, and the
// throttling lines may be absent when the cpu controller is off.
func ReadCPUStat(path string) (CPUStat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CPUStat{}, err
	}
	var s CPUStat
	fields := map[string]*uint64{
		"usage_usec":     &s.UsageUsec,
		"nr_periods":     &s.NrPeriods,
		"nr_throttled":   &s.NrThrottled,
		"throttled_usec": &s.ThrottledUsec,
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		dst, wanted := fields[key]
		if !wanted {
			continue
		}
		if *dst, err = strconv.ParseUint(value, 10, 64); err != nil {
			return CPUStat{}, fmt.Errorf("%s: bad %s %q", path, key, value)
		}
	}
	return s, nil
}

// ReadMemAnon returns the anon line of a memory.stat file: memory the kernel can't
// simply drop, unlike the page cache that memory.current also counts.
func ReadMemAnon(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "anon "); ok {
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", path, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s: no anon line", path)
}

// ReadUint reads a file holding one number, such as memory.current.
func ReadUint(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}
