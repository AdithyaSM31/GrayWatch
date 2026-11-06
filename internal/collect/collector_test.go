package collect

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/lblog"
	"github.com/AdithyaSM31/graywatch/internal/netstat"
)

var _ ClientSource = (*lblog.Window)(nil)

type fakeKernel struct {
	snaps map[string]KernelSnap
	errs  map[string]error
}

func (k *fakeKernel) Read(h fleet.Host) (KernelSnap, error) {
	if err := k.errs[h.Name]; err != nil {
		return KernelSnap{}, err
	}
	return k.snaps[h.Name], nil
}

type clientStats struct {
	p90, errRate float64
	n            int
}

type fakeClient struct {
	stats   map[string]clientStats
	gotSpan time.Duration
	gotNow  time.Time
}

func (c *fakeClient) Stats(server string, now time.Time, span time.Duration) (float64, float64, int) {
	c.gotSpan, c.gotNow = span, now
	s := c.stats[server]
	return s.p90, s.errRate, s.n
}

type rig struct {
	kernel *fakeKernel
	client *fakeClient
	clock  time.Time
	c      *Collector
}

var startTime = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func newRig(hosts ...string) *rig {
	r := &rig{
		kernel: &fakeKernel{snaps: map[string]KernelSnap{}, errs: map[string]error{}},
		client: &fakeClient{stats: map[string]clientStats{}},
		clock:  startTime,
	}
	r.c = New(r.kernel, r.client, func() time.Time { return r.clock })
	hs := make([]fleet.Host, len(hosts))
	for i, name := range hosts {
		hs[i] = fleet.Host{Name: name, PID: 100 + i}
	}
	r.c.SetHosts(hs)
	return r
}

// tick advances the clock by d, then ticks and returns the samples by host.
func (r *rig) tick(d time.Duration) map[string]Sample {
	r.clock = r.clock.Add(d)
	byHost := map[string]Sample{}
	for _, s := range r.c.Tick() {
		byHost[s.Host] = s
	}
	return byHost
}

func wantValue(t *testing.T, s Sample, sig Signal, want float64) {
	t.Helper()
	got, ok := s.Values[sig]
	if !ok {
		t.Fatalf("%s: %s missing, values = %v", s.Host, sig, s.Values)
	}
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s: %s = %v, want %v", s.Host, sig, got, want)
	}
}

func wantMissing(t *testing.T, s Sample, sigs ...Signal) {
	t.Helper()
	for _, sig := range sigs {
		if got, ok := s.Values[sig]; ok {
			t.Errorf("%s: %s = %v, want it missing", s.Host, sig, got)
		}
	}
}

var kernelSignals = []Signal{SigCPUPSI, SigIOPSI, SigMemPSI, SigThrottle, SigRetrans}

func TestSignalNames(t *testing.T) {
	for sig, want := range map[Signal]string{
		SigLatP90: "lat_p90_ms", SigErrRate: "err_rate", SigCPUPSI: "cpu_psi", SigIOPSI: "io_psi",
		SigMemPSI: "mem_psi", SigThrottle: "cpu_throttle", SigRetrans: "tcp_retrans",
	} {
		if string(sig) != want {
			t.Errorf("signal %q, want %q", sig, want)
		}
	}
}

func TestFirstTickHasNoKernelRates(t *testing.T) {
	r := newRig("app-01")
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: time.Hour}}
	s := r.tick(time.Second)["app-01"]
	wantMissing(t, s, kernelSignals...)
	if s.Values == nil {
		t.Fatal("Values is nil, want an empty map")
	}
}

func TestPSIRate(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{
		CPU: cgroup.PSI{SomeTotal: 300 * time.Millisecond},
		IO:  cgroup.PSI{SomeTotal: 200 * time.Millisecond},
		Mem: cgroup.PSI{SomeTotal: 100 * time.Millisecond},
	}
	s := r.tick(time.Second)["app-01"]
	wantValue(t, s, SigCPUPSI, 0.3)
	wantValue(t, s, SigIOPSI, 0.2)
	wantValue(t, s, SigMemPSI, 0.1)
}

// The rate must use the real time between snapshots, because a tick can run
// late while the daemon waits on a drain.
func TestRatesUseElapsedTime(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{
		CPU:     cgroup.PSI{SomeTotal: 300 * time.Millisecond},
		CPUStat: cgroup.CPUStat{ThrottledUsec: 500000},
	}
	s := r.tick(2 * time.Second)["app-01"]
	wantValue(t, s, SigCPUPSI, 0.15)
	wantValue(t, s, SigThrottle, 0.25)
}

func TestThrottleRate(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{CPUStat: cgroup.CPUStat{ThrottledUsec: 500000}}
	wantValue(t, r.tick(time.Second)["app-01"], SigThrottle, 0.5)
}

func TestRetransNeedsMinSegs(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)

	r.kernel.snaps["app-01"] = KernelSnap{TCP: netstat.TCPCounters{OutSegs: 19, RetransSegs: 5}}
	wantMissing(t, r.tick(time.Second)["app-01"], SigRetrans)

	r.kernel.snaps["app-01"] = KernelSnap{TCP: netstat.TCPCounters{OutSegs: 39, RetransSegs: 6}}
	wantValue(t, r.tick(time.Second)["app-01"], SigRetrans, 1.0/20)

	r.kernel.snaps["app-01"] = KernelSnap{TCP: netstat.TCPCounters{OutSegs: 239, RetransSegs: 16}}
	wantValue(t, r.tick(time.Second)["app-01"], SigRetrans, 0.05)
}

func TestClientSignalsNeedMinRequests(t *testing.T) {
	r := newRig("app-01")
	r.client.stats["app-01"] = clientStats{p90: 12, errRate: 0.25, n: 19}
	wantMissing(t, r.tick(time.Second)["app-01"], SigLatP90, SigErrRate)

	r.client.stats["app-01"] = clientStats{p90: 12, errRate: 0.25, n: 20}
	s := r.tick(time.Second)["app-01"]
	wantValue(t, s, SigLatP90, 12)
	wantValue(t, s, SigErrRate, 0.25)
}

func TestClientSignalsUseFiveSecondWindow(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	if r.client.gotSpan != 5*time.Second {
		t.Fatalf("span = %v, want 5s", r.client.gotSpan)
	}
	if !r.client.gotNow.Equal(startTime.Add(time.Second)) {
		t.Fatalf("now = %v, want the collector's clock", r.client.gotNow)
	}
}

// Every counter gets its own case: a restart resets them all, but a check that
// only covered some would still emit a huge rate from the others.
func TestCounterResetSkipsSample(t *testing.T) {
	high := KernelSnap{
		CPU:     cgroup.PSI{SomeTotal: time.Hour},
		IO:      cgroup.PSI{SomeTotal: time.Hour},
		Mem:     cgroup.PSI{SomeTotal: time.Hour},
		CPUStat: cgroup.CPUStat{ThrottledUsec: 1 << 40},
		TCP:     netstat.TCPCounters{OutSegs: 1 << 40, RetransSegs: 1 << 40},
	}
	for name, reset := range map[string]func(*KernelSnap){
		"cpu psi":   func(s *KernelSnap) { s.CPU.SomeTotal = 0 },
		"io psi":    func(s *KernelSnap) { s.IO.SomeTotal = 0 },
		"mem psi":   func(s *KernelSnap) { s.Mem.SomeTotal = 0 },
		"throttled": func(s *KernelSnap) { s.CPUStat.ThrottledUsec = 0 },
		"out segs":  func(s *KernelSnap) { s.TCP.OutSegs = 0 },
		"retrans":   func(s *KernelSnap) { s.TCP.RetransSegs = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig("app-01")
			r.kernel.snaps["app-01"] = high
			r.tick(time.Second)

			after := high
			reset(&after)
			r.kernel.snaps["app-01"] = after
			wantMissing(t, r.tick(time.Second)["app-01"], kernelSignals...)

			// The reset value is the new base, so the next tick has real rates.
			next := after
			next.CPU.SomeTotal += 300 * time.Millisecond
			r.kernel.snaps["app-01"] = next
			wantValue(t, r.tick(time.Second)["app-01"], SigCPUPSI, 0.3)
		})
	}
}

func TestCounterResetKeepsClientSignals(t *testing.T) {
	r := newRig("app-01")
	r.client.stats["app-01"] = clientStats{p90: 12, n: 50}
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: time.Hour}}
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{}
	s := r.tick(time.Second)["app-01"]
	wantMissing(t, s, kernelSignals...)
	wantValue(t, s, SigLatP90, 12)
}

func TestPIDChangeDropsSnapshot(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: time.Second}}

	r.c.SetHosts([]fleet.Host{{Name: "app-01", PID: 999}})
	wantMissing(t, r.tick(time.Second)["app-01"], kernelSignals...)

	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: 1300 * time.Millisecond}}
	wantValue(t, r.tick(time.Second)["app-01"], SigCPUPSI, 0.3)
}

func TestSamePIDKeepsSnapshot(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.c.SetHosts([]fleet.Host{{Name: "app-01", PID: 100}})
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: 300 * time.Millisecond}}
	wantValue(t, r.tick(time.Second)["app-01"], SigCPUPSI, 0.3)
}

func TestRemovedHostStopsReporting(t *testing.T) {
	r := newRig("app-01", "app-02")
	r.tick(time.Second)
	r.c.SetHosts([]fleet.Host{{Name: "app-01", PID: 100}})
	if _, ok := r.tick(time.Second)["app-02"]; ok {
		t.Fatal("removed host still reported")
	}

	// Coming back with the same PID is still a gap, so it starts from scratch.
	r.c.SetHosts([]fleet.Host{{Name: "app-01", PID: 100}, {Name: "app-02", PID: 101}})
	r.kernel.snaps["app-02"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: time.Hour}}
	wantMissing(t, r.tick(time.Second)["app-02"], kernelSignals...)
}

func TestReadErrorOmitsOnlyThatHost(t *testing.T) {
	r := newRig("app-01", "app-02")
	r.client.stats["app-01"] = clientStats{p90: 12, n: 50}
	r.tick(time.Second)

	r.kernel.errs["app-01"] = errors.New("cgroup gone")
	r.kernel.snaps["app-02"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: 300 * time.Millisecond}}
	got := r.tick(time.Second)

	wantMissing(t, got["app-01"], kernelSignals...)
	wantValue(t, got["app-01"], SigLatP90, 12)
	wantValue(t, got["app-02"], SigCPUPSI, 0.3)
}

// The rate after a failed read spans both ticks, so it stays a true average.
func TestReadErrorKeepsLastGoodSnapshot(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.errs["app-01"] = errors.New("busy")
	r.tick(time.Second)
	delete(r.kernel.errs, "app-01")
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: 600 * time.Millisecond}}
	wantValue(t, r.tick(time.Second)["app-01"], SigCPUPSI, 0.3)
}

func TestClockNotAdvancingGivesNoRates(t *testing.T) {
	r := newRig("app-01")
	r.tick(time.Second)
	r.kernel.snaps["app-01"] = KernelSnap{CPU: cgroup.PSI{SomeTotal: time.Second}}
	s := r.tick(0)["app-01"]
	wantMissing(t, s, kernelSignals...)
	for sig, v := range s.Values {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			t.Errorf("%s = %v", sig, v)
		}
	}
}

func TestSamplesFollowHostOrderAndCarryTime(t *testing.T) {
	r := newRig("app-01", "app-02", "app-03")
	r.clock = r.clock.Add(time.Second)
	samples := r.c.Tick()
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(samples))
	}
	for i, want := range []string{"app-01", "app-02", "app-03"} {
		if samples[i].Host != want {
			t.Errorf("sample %d host = %q, want %q", i, samples[i].Host, want)
		}
		if !samples[i].At.Equal(r.clock) {
			t.Errorf("sample %d At = %v, want %v", i, samples[i].At, r.clock)
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fsFixture(t *testing.T) (FSKernelSource, fleet.Host) {
	t.Helper()
	cgroupDir, proc := t.TempDir(), t.TempDir()
	writeFiles(t, cgroupDir, map[string]string{
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=1000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"io.pressure":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=2000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2\n",
		"memory.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=3000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=3\n",
		"cpu.stat":        "usage_usec 9\nnr_periods 8\nnr_throttled 7\nthrottled_usec 4000\n",
	})
	writeFiles(t, proc, map[string]string{
		"42/net/snmp": "Tcp: OutSegs RetransSegs\nTcp: 500 6\n",
	})
	return FSKernelSource{ProcRoot: proc}, fleet.Host{Name: "app-01", PID: 42, CgroupDir: cgroupDir}
}

func TestFSKernelSourceReadsEachFile(t *testing.T) {
	src, h := fsFixture(t)
	got, err := src.Read(h)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPU.SomeTotal != time.Millisecond || got.IO.SomeTotal != 2*time.Millisecond || got.Mem.SomeTotal != 3*time.Millisecond {
		t.Errorf("PSI = %+v %+v %+v", got.CPU, got.IO, got.Mem)
	}
	if got.CPUStat.ThrottledUsec != 4000 {
		t.Errorf("CPUStat = %+v", got.CPUStat)
	}
	if got.TCP != (netstat.TCPCounters{OutSegs: 500, RetransSegs: 6}) {
		t.Errorf("TCP = %+v", got.TCP)
	}
}

func TestFSKernelSourceFailsWhenAnyFileIsMissing(t *testing.T) {
	for _, name := range []string{"cpu.pressure", "io.pressure", "memory.pressure", "cpu.stat"} {
		src, h := fsFixture(t)
		if err := os.Remove(filepath.Join(h.CgroupDir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := src.Read(h); err == nil {
			t.Errorf("%s missing: want error", name)
		}
	}
	src, h := fsFixture(t)
	if err := os.Remove(filepath.Join(src.ProcRoot, "42", "net", "snmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Read(h); err == nil {
		t.Error("snmp missing: want error")
	}
}
