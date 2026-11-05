package detect

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/lblog"
)

// These tests run the real request window, collector and detector together on a
// simulated clock. The normal numbers are from the live check on Docker Desktop:
// about 30 requests per second per host with a p90 near 16 ms, and a leaked pool
// answering 500 after about 1010 ms.

// idleKernel reports no pressure at all, so these tests exercise client signals.
type idleKernel struct{}

func (idleKernel) Read(fleet.Host) (collect.KernelSnap, error) { return collect.KernelSnap{}, nil }

type simRequest struct {
	host   string
	tookMs int
	status int
}

type simulation struct {
	t         *testing.T
	rng       *rand.Rand
	clock     time.Time
	window    *lblog.Window
	collector *collect.Collector
	detector  *Detector
	hosts     []string
	pending   map[time.Time][]lblog.Req
}

func newSimulation(t *testing.T, seed int64) *simulation {
	t.Helper()
	s := &simulation{
		t:       t,
		rng:     rand.New(rand.NewSource(seed)),
		clock:   startTime,
		window:  lblog.NewWindow(time.Minute),
		pending: map[time.Time][]lblog.Req{},
	}
	var hosts []fleet.Host
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("app-%02d", i)
		s.hosts = append(s.hosts, name)
		hosts = append(hosts, fleet.Host{Name: name, PID: i})
	}
	s.collector = collect.New(idleKernel{}, s.window, func() time.Time { return s.clock })
	s.collector.SetHosts(hosts)
	s.detector = New(DefaultConfig())
	return s
}

// second simulates one second of traffic, then runs one detection tick. HAProxy
// logs a request when it finishes, so a slow one reaches the window late.
func (s *simulation) second(behave func(host string) simRequest) Observation {
	const perSecond = 30
	for _, h := range s.hosts {
		for i := 0; i < perSecond; i++ {
			r := behave(h)
			offset := time.Duration(i) * time.Second / perSecond
			done := s.clock.Add(offset + time.Duration(r.tookMs)*time.Millisecond).Truncate(100 * time.Millisecond)
			s.pending[done] = append(s.pending[done], lblog.Req{Server: h, TrMs: r.tookMs, Status: r.status, At: done})
		}
	}
	s.clock = s.clock.Add(time.Second)
	for at, reqs := range s.pending {
		if !at.After(s.clock) {
			for _, r := range reqs {
				s.window.Add(r)
			}
			delete(s.pending, at)
		}
	}
	return s.detector.Observe(s.collector.Tick(), nil)
}

func (s *simulation) healthy(host string) simRequest {
	return simRequest{host: host, tookMs: 12 + s.rng.Intn(9), status: 200}
}

func TestPipelineHealthyFleetHasNoFalsePositives(t *testing.T) {
	s := newSimulation(t, 1)
	for i := 0; i < 600; i++ {
		obs := s.second(s.healthy)
		if got := suspects(obs); len(got) != 0 {
			t.Fatalf("second %d: suspects = %v", i, got)
		}
		if obs.Fleet != nil {
			t.Fatalf("second %d: fleet alert %+v", i, obs.Fleet)
		}
	}
}

func TestPipelineCatchesPoolLeakWithinSeconds(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		s := newSimulation(t, seed)
		for i := 0; i < 30; i++ {
			s.second(s.healthy)
		}

		leak := func(host string) simRequest {
			if host == "app-07" {
				return simRequest{host: host, tookMs: 1010, status: 500}
			}
			return s.healthy(host)
		}
		detectedAfter := -1
		for i := 1; i <= 30; i++ {
			obs := s.second(leak)
			got := suspects(obs)
			if len(got) > 1 || (len(got) == 1 && got[0] != "app-07") {
				t.Fatalf("seed %d, second %d: suspects = %v, want only app-07", seed, i, got)
			}
			if obs.Fleet != nil {
				t.Fatalf("seed %d, second %d: fleet alert %+v from one sick host", seed, i, obs.Fleet)
			}
			if len(got) == 1 {
				v := verdict(t, obs, "app-07")
				if v.Category != "app" {
					t.Errorf("seed %d: category = %q, want app", seed, v.Category)
				}
				detectedAfter = i
				break
			}
		}
		if detectedAfter < 0 {
			t.Fatalf("seed %d: not detected within 30 s", seed)
		}
		// The plan's end-to-end step expects suspect and drain within about 10 s.
		if detectedAfter > 10 {
			t.Errorf("seed %d: detected after %d s, want within 10", seed, detectedAfter)
		}
		t.Logf("seed %d: detected %d s after the leak began", seed, detectedAfter)
	}
}

func TestPipelineRecoversAfterFaultEnds(t *testing.T) {
	s := newSimulation(t, 2)
	for i := 0; i < 30; i++ {
		s.second(s.healthy)
	}
	for i := 0; i < 20; i++ {
		s.second(func(host string) simRequest {
			if host == "app-07" {
				return simRequest{host: host, tookMs: 1010, status: 500}
			}
			return s.healthy(host)
		})
	}
	recoveredAfter := -1
	for i := 1; i <= 30; i++ {
		obs := s.second(s.healthy)
		if verdict(t, obs, "app-07").State == Healthy {
			recoveredAfter = i
			break
		}
	}
	if recoveredAfter < 0 {
		t.Fatal("still suspect 30 s after the fault ended")
	}
	t.Logf("recovered %d s after the fault ended", recoveredAfter)
}

// A slow shared dependency moves every host together: alert, and blame nobody.
func TestPipelineSharedDependencySlowdownBlamesNobody(t *testing.T) {
	s := newSimulation(t, 3)
	for i := 0; i < 30; i++ {
		s.second(s.healthy)
	}
	alerted := false
	for i := 0; i < 60; i++ {
		obs := s.second(func(host string) simRequest {
			return simRequest{host: host, tookMs: 620 + s.rng.Intn(30), status: 200}
		})
		if obs.Fleet != nil {
			alerted = true
		}
		if alerted && len(suspects(obs)) != 0 {
			t.Fatalf("second %d: suspects = %v during a fleet-wide slowdown", i, suspects(obs))
		}
	}
	if !alerted {
		t.Fatal("no fleet alert")
	}
}
