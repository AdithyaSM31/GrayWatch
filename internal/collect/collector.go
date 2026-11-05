// Package collect turns cumulative kernel counters and the request log into the
// per-tick signals the detector compares across the fleet.
package collect

import (
	"time"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

const (
	clientSpan = 5 * time.Second

	// Below these counts a ratio is mostly noise: one failure in five requests
	// would read as a 20% error rate.
	minClientRequests = 20
	minOutSegs        = 20
)

type Signal string

const (
	SigLatP90   Signal = "lat_p90_ms"
	SigErrRate  Signal = "err_rate"
	SigCPUPSI   Signal = "cpu_psi"
	SigIOPSI    Signal = "io_psi"
	SigMemPSI   Signal = "mem_psi"
	SigThrottle Signal = "cpu_throttle"
	SigRetrans  Signal = "tcp_retrans"
)

// Sample is one host's signals for one tick. A missing key means no data, which
// is different from 0: a drained host has no latency, not a latency of zero.
type Sample struct {
	Host   string
	At     time.Time
	Values map[Signal]float64
}

type snapshot struct {
	pid  int
	at   time.Time
	snap KernelSnap
}

// Collector is not safe for concurrent use; the daemon calls it from one loop.
type Collector struct {
	kernel KernelSource
	client ClientSource
	now    func() time.Time
	hosts  []fleet.Host
	prev   map[string]snapshot
}

func New(k KernelSource, c ClientSource, now func() time.Time) *Collector {
	return &Collector{kernel: k, client: c, now: now, prev: map[string]snapshot{}}
}

// SetHosts replaces the hosts to collect from. A host whose PID changed has
// restarted, so its counters have no relation to the previous snapshot.
func (c *Collector) SetHosts(hs []fleet.Host) {
	prev := make(map[string]snapshot, len(hs))
	for _, h := range hs {
		if p, ok := c.prev[h.Name]; ok && p.pid == h.PID {
			prev[h.Name] = p
		}
	}
	c.hosts, c.prev = hs, prev
}

// Tick reads every host once. A host's kernel signals are left out on its first
// tick, on a read error, and when a counter went backwards. The client signals
// come from a separate source and are not affected.
func (c *Collector) Tick() []Sample {
	now := c.now()
	samples := make([]Sample, 0, len(c.hosts))
	for _, h := range c.hosts {
		values := map[Signal]float64{}
		if snap, err := c.kernel.Read(h); err == nil {
			if p, ok := c.prev[h.Name]; ok {
				addKernelRates(values, p.snap, snap, now.Sub(p.at))
			}
			c.prev[h.Name] = snapshot{pid: h.PID, at: now, snap: snap}
		}
		if p90, errRate, n := c.client.Stats(h.Name, now, clientSpan); n >= minClientRequests {
			values[SigLatP90] = p90
			values[SigErrRate] = errRate
		}
		samples = append(samples, Sample{Host: h.Name, At: now, Values: values})
	}
	return samples
}

func addKernelRates(values map[Signal]float64, prev, cur KernelSnap, dt time.Duration) {
	if dt <= 0 || wentBackwards(prev, cur) {
		return
	}
	seconds := dt.Seconds()
	values[SigCPUPSI] = (cur.CPU.SomeTotal - prev.CPU.SomeTotal).Seconds() / seconds
	values[SigIOPSI] = (cur.IO.SomeTotal - prev.IO.SomeTotal).Seconds() / seconds
	values[SigMemPSI] = (cur.Mem.SomeTotal - prev.Mem.SomeTotal).Seconds() / seconds
	values[SigThrottle] = float64(cur.CPUStat.ThrottledUsec-prev.CPUStat.ThrottledUsec) / 1e6 / seconds
	if out := cur.TCP.OutSegs - prev.TCP.OutSegs; out >= minOutSegs {
		values[SigRetrans] = float64(cur.TCP.RetransSegs-prev.TCP.RetransSegs) / float64(out)
	}
}

// wentBackwards catches a counter reset. The counters are unsigned, so without
// this check a reset would wrap around to an enormous rate.
func wentBackwards(prev, cur KernelSnap) bool {
	return cur.CPU.SomeTotal < prev.CPU.SomeTotal ||
		cur.IO.SomeTotal < prev.IO.SomeTotal ||
		cur.Mem.SomeTotal < prev.Mem.SomeTotal ||
		cur.CPUStat.ThrottledUsec < prev.CPUStat.ThrottledUsec ||
		cur.TCP.OutSegs < prev.TCP.OutSegs ||
		cur.TCP.RetransSegs < prev.TCP.RetransSegs
}
