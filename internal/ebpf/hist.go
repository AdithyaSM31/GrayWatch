// Package ebpf measures run queue latency per cgroup: how long a task waited for a
// CPU after it was ready to run. PSI says a cgroup was stalled for some share of
// the time, and this says how long each wait was.
package ebpf

const (
	// Slots is the number of log2 buckets, which must match runqlat.bpf.c. Slot i
	// holds latencies of 2^i to 2^(i+1) microseconds.
	Slots = 32

	// MinEvents is the fewest scheduling events worth a p99. Below this, the p99 is
	// just the slowest event: 20 is 1 event per 50 ms over a 1 s tick.
	MinEvents = 20
)

// P99 returns the upper bound of the bucket holding the nearest-rank 99th
// percentile, in microseconds. It reports false with too few events.
func P99(counts [Slots]uint64) (float64, bool) {
	var total uint64
	for _, n := range counts {
		total += n
	}
	if total < MinEvents {
		return 0, false
	}
	rank := (99*total + 99) / 100 // nearest rank: ceil(0.99*n)
	var seen uint64
	for i, n := range counts {
		seen += n
		if seen >= rank {
			return float64(uint64(1) << (i + 1)), true
		}
	}
	return 0, false // not reachable: rank <= total
}

// Delta returns what was added between two reads of a cumulative histogram.
func Delta(cur, prev [Slots]uint64) [Slots]uint64 {
	var d [Slots]uint64
	for i := range cur {
		if cur[i] > prev[i] {
			d[i] = cur[i] - prev[i]
		}
	}
	return d
}
