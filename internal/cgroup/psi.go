// Package cgroup reads per-container kernel signals from cgroup v2 files.
package cgroup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// PSI holds the cumulative stall times from a pressure file. Only the totals
// are kept: the kernel's avg10 is too slow for a 1 s tick, so the collector
// computes its own rates from these.
type PSI struct {
	SomeTotal, FullTotal time.Duration
}

// ReadPSI parses a pressure file. A missing "full" line gives FullTotal 0.
func ReadPSI(path string) (PSI, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PSI{}, err
	}
	var psi PSI
	sawSome := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		total, err := parseTotal(fields)
		if err != nil {
			return PSI{}, fmt.Errorf("%s: %w", path, err)
		}
		switch fields[0] {
		case "some":
			psi.SomeTotal = total
			sawSome = true
		case "full":
			psi.FullTotal = total
		}
	}
	if !sawSome {
		return PSI{}, fmt.Errorf("%s: no \"some\" line", path)
	}
	return psi, nil
}

func parseTotal(fields []string) (time.Duration, error) {
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "total="); ok {
			usec, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("bad total %q", v)
			}
			return time.Duration(usec) * time.Microsecond, nil
		}
	}
	return 0, fmt.Errorf("no total in %q", strings.Join(fields, " "))
}
