package collect

import (
	"errors"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

type fakeRunq struct {
	p99s map[uint64]float64
	err  error
}

func (f fakeRunq) P99ByCgroup() (map[uint64]float64, error) { return f.p99s, f.err }

type emptyKernel struct{}

func (emptyKernel) Read(fleet.Host) (KernelSnap, error) { return KernelSnap{}, nil }

type noClient struct{}

func (noClient) Stats(string, time.Time, time.Duration) (float64, float64, int) { return 0, 0, 0 }

func newRunqCollector(src RunqSource) *Collector {
	c := New(emptyKernel{}, noClient{}, func() time.Time { return time.Unix(100, 0) })
	c.SetRunq(src, func(dir string) (uint64, error) {
		switch dir {
		case "/cg/a":
			return 11, nil
		case "/cg/b":
			return 22, nil
		}
		return 0, errors.New("no such cgroup")
	})
	c.SetHosts([]fleet.Host{{Name: "a", CgroupDir: "/cg/a"}, {Name: "b", CgroupDir: "/cg/b"}, {Name: "c", CgroupDir: "/cg/gone"}})
	return c
}

func sampleOf(t *testing.T, samples []Sample, host string) Sample {
	t.Helper()
	for _, s := range samples {
		if s.Host == host {
			return s
		}
	}
	t.Fatalf("no sample for %s", host)
	return Sample{}
}

func TestRunqSignalIsMappedByCgroupID(t *testing.T) {
	c := newRunqCollector(fakeRunq{p99s: map[uint64]float64{11: 4096, 22: 16}})
	samples := c.Tick()
	if got := sampleOf(t, samples, "a").Values[SigRunqP99]; got != 4096 {
		t.Errorf("a = %v", got)
	}
	if got := sampleOf(t, samples, "b").Values[SigRunqP99]; got != 16 {
		t.Errorf("b = %v", got)
	}
}

// No events means no data, not a latency of zero, like a drained host's latency.
func TestRunqSignalIsMissingWithoutData(t *testing.T) {
	c := newRunqCollector(fakeRunq{p99s: map[uint64]float64{11: 4096}})
	samples := c.Tick()
	if _, ok := sampleOf(t, samples, "b").Values[SigRunqP99]; ok {
		t.Error("b had no events")
	}
	if _, ok := sampleOf(t, samples, "c").Values[SigRunqP99]; ok {
		t.Error("c has no cgroup id")
	}
}

func TestRunqReadErrorLeavesOtherSignalsAlone(t *testing.T) {
	c := newRunqCollector(fakeRunq{err: errors.New("map closed")})
	if got := c.Tick(); len(got) != 3 {
		t.Fatalf("got %d samples", len(got))
	}
}

func TestNoRunqSourceMeansNoSignal(t *testing.T) {
	c := New(emptyKernel{}, noClient{}, func() time.Time { return time.Unix(100, 0) })
	c.SetHosts([]fleet.Host{{Name: "a", CgroupDir: "/cg/a"}})
	if _, ok := c.Tick()[0].Values[SigRunqP99]; ok {
		t.Fatal("signal without a source")
	}
}

// A container that restarts gets a new cgroup, so the id can't be kept forever.
func TestRunqLooksTheCgroupUpAgainAfterAChange(t *testing.T) {
	ids := map[string]uint64{"/cg/a": 11}
	c := New(emptyKernel{}, noClient{}, func() time.Time { return time.Unix(100, 0) })
	c.SetRunq(fakeRunq{p99s: map[uint64]float64{11: 8, 99: 32}}, func(dir string) (uint64, error) { return ids[dir], nil })
	c.SetHosts([]fleet.Host{{Name: "a", CgroupDir: "/cg/a", PID: 1}})
	if got := c.Tick()[0].Values[SigRunqP99]; got != 8 {
		t.Fatalf("got %v", got)
	}
	ids["/cg/a"] = 99
	c.SetHosts([]fleet.Host{{Name: "a", CgroupDir: "/cg/a", PID: 2}})
	if got := c.Tick()[0].Values[SigRunqP99]; got != 32 {
		t.Fatalf("got %v", got)
	}
}
