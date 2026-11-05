package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/detect"
	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/lblog"
	"github.com/AdithyaSM31/graywatch/internal/metrics"
	"github.com/AdithyaSM31/graywatch/internal/remediate"
)

var startTime = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

type recordingActuator struct {
	calls []string
	fail  func(call string) error
}

func (a *recordingActuator) do(call string) error {
	a.calls = append(a.calls, call)
	if a.fail != nil {
		return a.fail(call)
	}
	return nil
}

func (a *recordingActuator) Drain(_ context.Context, host string) error {
	return a.do("drain " + host)
}

func (a *recordingActuator) Restore(_ context.Context, host string) error {
	return a.do("restore " + host)
}

type idleKernel struct{}

func (idleKernel) Read(fleet.Host) (collect.KernelSnap, error) { return collect.KernelSnap{}, nil }

// sim is a daemon with everything real except the kernel, HAProxy and the
// traffic. It routes simulated requests away from hosts the planner has drained,
// as HAProxy would. Normal numbers are from the live check on Docker Desktop.
type sim struct {
	t        *testing.T
	d        *Daemon
	rng      *rand.Rand
	clock    time.Time
	window   *lblog.Window
	actuator *recordingActuator
	planner  *remediate.Planner
	log      bytes.Buffer
	hosts    []fleet.Host
	discover func(context.Context) ([]fleet.Host, error)
	metrics  *metrics.Metrics
}

func newSim(t *testing.T, policy remediate.Policy) *sim {
	t.Helper()
	s := &sim{
		t:        t,
		rng:      rand.New(rand.NewSource(1)),
		clock:    startTime,
		window:   lblog.NewWindow(time.Minute),
		actuator: &recordingActuator{},
		metrics:  metrics.New(prometheus.NewRegistry()),
	}
	for i := 1; i <= 20; i++ {
		s.hosts = append(s.hosts, fleet.Host{Name: fmt.Sprintf("app-%02d", i), PID: i})
	}
	now := func() time.Time { return s.clock }
	s.discover = func(context.Context) ([]fleet.Host, error) { return s.hosts, nil }
	s.planner = remediate.NewPlanner(policy, 20, nil, now())
	s.d = New(Deps{
		Discover:  func(ctx context.Context) ([]fleet.Host, error) { return s.discover(ctx) },
		Collector: collect.New(idleKernel{}, s.window, now),
		Detector:  detect.New(detect.DefaultConfig()),
		Planner:   s.planner,
		Actuator:  s.actuator,
		Events:    events.NewWriter(&s.log),
		Metrics:   s.metrics,
		Now:       now,
	})
	return s
}

type behaviour func(host string) (trMs, status int)

func (s *sim) healthy(string) (int, int) { return 12 + s.rng.Intn(9), 200 }

func (s *sim) leakOn(target string) behaviour {
	return func(host string) (int, int) {
		if host == target {
			return 1010, 500
		}
		return s.healthy(host)
	}
}

// run simulates n seconds of traffic with one daemon step per second.
func (s *sim) run(n int, b behaviour) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		drained := s.planner.Drained()
		for _, h := range s.hosts {
			if drained[h.Name] {
				continue
			}
			for r := 0; r < 30; r++ {
				tr, status := b(h.Name)
				s.window.Add(lblog.Req{Server: h.Name, TrMs: tr, Status: status, At: s.clock})
			}
		}
		s.clock = s.clock.Add(time.Second)
		s.d.Step(context.Background())
	}
}

func (s *sim) events() []events.Event {
	s.t.Helper()
	out, err := events.ReadAll(bytes.NewReader(s.log.Bytes()))
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *sim) count(eventType string) int {
	s.t.Helper()
	n := 0
	for _, e := range s.events() {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

func (s *sim) first(eventType string) (events.Event, bool) {
	s.t.Helper()
	for _, e := range s.events() {
		if e.Type == eventType {
			return e, true
		}
	}
	return events.Event{}, false
}

func TestHealthyFleetDoesNothing(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(120, s.healthy)
	if len(s.actuator.calls) != 0 {
		t.Fatalf("calls = %q", s.actuator.calls)
	}
	if got := s.events(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
}

// The plan's end-to-end step: a pool leak gives suspect, then drain, within
// about 10 s, and the host comes back after the fault ends and the cooldown.
func TestPoolLeakIsDrainedThenRestored(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(30, s.healthy)
	faultAt := s.clock

	s.run(15, s.leakOn("app-07"))
	suspect, ok := s.first(events.Suspect)
	if !ok || suspect.Host != "app-07" || suspect.Category != "app" || len(suspect.Signals) == 0 || suspect.Score <= 0 {
		t.Fatalf("suspect event = %+v, %v", suspect, ok)
	}
	drain, ok := s.first(events.Drain)
	if !ok || drain.Host != "app-07" || drain.DryRun {
		t.Fatalf("drain event = %+v, %v", drain, ok)
	}
	if took := drain.TS.Sub(faultAt); took > 10*time.Second {
		t.Fatalf("drained %v after the fault began, want within 10s", took)
	}
	t.Logf("suspect after %v, drain after %v", suspect.TS.Sub(faultAt), drain.TS.Sub(faultAt))
	if drain.Category != "app" || len(drain.Signals) == 0 {
		t.Errorf("drain event lacks the reason: %+v", drain)
	}
	if !slices.Equal(s.actuator.calls, []string{"drain app-07"}) {
		t.Fatalf("calls = %q", s.actuator.calls)
	}

	// Traffic has left app-07, so its client signals age out and it reads healthy.
	s.run(61, s.healthy)
	if !slices.Contains(s.actuator.calls, "restore app-07") {
		t.Fatalf("calls = %q, want a restore after the cooldown", s.actuator.calls)
	}
	if s.count(events.Restore) != 1 || s.count(events.Healthy) != 1 {
		t.Fatalf("events = %+v", s.events())
	}
	if s.count(events.Drain) != 1 {
		t.Fatalf("drained %d times, want once", s.count(events.Drain))
	}
}

// A fault that outlasts the cooldown keeps coming back, and flap protection has
// to end the cycle with the host out of rotation.
func TestPersistentFaultEndsInFlappingAlert(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(10, s.healthy)
	s.run(9*60, s.leakOn("app-07"))

	if got := s.count(events.Drain); got != 3 {
		t.Fatalf("drained %d times, want 3: %q", got, s.actuator.calls)
	}
	if got := s.count(events.Flapping); got != 1 {
		t.Fatalf("flapping alerts = %d, want 1", got)
	}
	if !s.planner.Drained()["app-07"] {
		t.Fatal("flapping host is back in rotation")
	}
}

// The plan's dep-slow check: every host slows together, so alert and drain nothing.
func TestSharedSlowdownAlertsAndDrainsNothing(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(30, s.healthy)
	s.run(60, func(string) (int, int) { return 620 + s.rng.Intn(30), 200 })

	if s.count(events.FleetAlert) == 0 {
		t.Fatal("no fleet alert")
	}
	if len(s.actuator.calls) != 0 {
		t.Fatalf("calls = %q, want none", s.actuator.calls)
	}
}

// Review Focus 4: a failed drain is reported, not counted, and retried.
func TestFailedDrainIsReportedAndRetried(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	failures := 2
	s.actuator.fail = func(string) error {
		if failures > 0 {
			failures--
			return errors.New("runtime API down")
		}
		return nil
	}
	s.run(30, s.healthy)
	s.run(15, s.leakOn("app-07"))

	if got := len(s.actuator.calls); got != 3 {
		t.Fatalf("calls = %q, want 2 failures then a success", s.actuator.calls)
	}
	if got := s.count(events.ActionError); got != 2 {
		t.Fatalf("action errors = %d, want 2", got)
	}
	e, _ := s.first(events.ActionError)
	if e.Host != "app-07" || !strings.Contains(e.Reason, "runtime API down") {
		t.Errorf("action error = %+v", e)
	}
	if got := s.count(events.Drain); got != 1 {
		t.Fatalf("drain events = %d, want only the one that worked", got)
	}
	if !s.planner.Drained()["app-07"] {
		t.Fatal("not drained after the retry worked")
	}
}

func TestDryRunNeverCallsActuator(t *testing.T) {
	p := remediate.DefaultPolicy()
	p.DryRun = true
	s := newSim(t, p)
	s.run(30, s.healthy)
	s.run(30, s.leakOn("app-07"))

	if len(s.actuator.calls) != 0 {
		t.Fatalf("calls = %q, want none", s.actuator.calls)
	}
	drain, ok := s.first(events.Drain)
	if !ok || !drain.DryRun || drain.Host != "app-07" {
		t.Fatalf("drain event = %+v, %v", drain, ok)
	}
	// Nothing changes in a dry run, so the planner proposes it again every tick.
	// One event says it all; 30 would read as 30 drains.
	if got := s.count(events.Drain); got != 1 {
		t.Fatalf("drain events = %d, want 1", got)
	}
}

func TestDryRunReportsAgainAfterTheHostRecovers(t *testing.T) {
	p := remediate.DefaultPolicy()
	p.DryRun = true
	s := newSim(t, p)
	s.run(10, s.healthy)
	s.run(15, s.leakOn("app-07"))
	s.run(15, s.healthy)
	s.run(15, s.leakOn("app-07"))
	if got := s.count(events.Drain); got != 2 {
		t.Fatalf("drain events = %d, want 2", got)
	}
}

// A long outage must not write one event per second.
func TestRepeatedAlertsAreReportedOnce(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(10, s.healthy)
	s.run(60, func(string) (int, int) { return 620 + s.rng.Intn(30), 200 })
	if got := s.count(events.FleetAlert); got != 1 {
		t.Fatalf("fleet alerts = %d, want 1 for one continuous outage", got)
	}
	s.run(30, s.healthy)
	s.run(30, func(string) (int, int) { return 620 + s.rng.Intn(30), 200 })
	if got := s.count(events.FleetAlert); got != 2 {
		t.Fatalf("fleet alerts = %d, want 2 for two outages", got)
	}
}

func TestSuspectAndHealthyEmittedOnTransitionsOnly(t *testing.T) {
	p := remediate.DefaultPolicy()
	p.DryRun = true
	s := newSim(t, p)
	s.run(10, s.healthy)
	s.run(30, s.leakOn("app-07"))
	if got := s.count(events.Suspect); got != 1 {
		t.Fatalf("suspect events = %d, want 1", got)
	}
	s.run(30, s.healthy)
	if got := s.count(events.Healthy); got != 1 {
		t.Fatalf("healthy events = %d, want 1", got)
	}
}

func TestRediscoversOnSchedule(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	calls := 0
	s.discover = func(context.Context) ([]fleet.Host, error) {
		calls++
		return s.hosts, nil
	}
	s.run(61, s.healthy)
	// At the first step, then every 30 s.
	if calls != 3 {
		t.Fatalf("discover calls = %d, want 3", calls)
	}
}

// A docker hiccup must not make the daemon forget the fleet, which would read as
// every host leaving.
func TestFailedDiscoveryKeepsCurrentHosts(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(5, s.healthy)
	s.discover = func(context.Context) ([]fleet.Host, error) { return nil, errors.New("docker down") }
	s.run(35, s.healthy)
	s.run(15, s.leakOn("app-07"))
	if !slices.Contains(s.actuator.calls, "drain app-07") {
		t.Fatalf("calls = %q, want detection to keep working", s.actuator.calls)
	}
}

func TestEmptyDiscoveryKeepsCurrentHosts(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.run(5, s.healthy)
	s.discover = func(context.Context) ([]fleet.Host, error) { return nil, nil }
	s.run(35, s.healthy)
	s.run(15, s.leakOn("app-07"))
	if !slices.Contains(s.actuator.calls, "drain app-07") {
		t.Fatalf("calls = %q, want detection to keep working", s.actuator.calls)
	}
}

func TestNoHostsAtAllDoesNothing(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.discover = func(context.Context) ([]fleet.Host, error) { return nil, errors.New("docker down") }
	for i := 0; i < 5; i++ {
		s.clock = s.clock.Add(time.Second)
		s.d.Step(context.Background())
	}
	if len(s.actuator.calls) != 0 || len(s.events()) != 0 {
		t.Fatalf("calls = %q, events = %+v", s.actuator.calls, s.events())
	}
}

func TestDrainedAtStartup(t *testing.T) {
	got := DrainedServers([]haproxy.ServerStat{
		{Name: "app-01", Status: "UP"},
		{Name: "app-02", Status: "DRAIN"},
		{Name: "app-03", Status: "MAINT"},
		{Name: "app-04", Status: "DOWN"},
		{Name: "app-05", Status: "UP 1/3"},
		{Name: "app-06", Status: "MAINT (resolution)"},
	})
	if want := []string{"app-02", "app-03", "app-06"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The plan's restart check: a host drained before a restart counts toward the
// cap and is not put straight back while it may still be sick.
func TestRestartKeepsDrainedHostOutForTheCooldown(t *testing.T) {
	s := newSim(t, remediate.DefaultPolicy())
	s.planner = remediate.NewPlanner(remediate.DefaultPolicy(), 20, []string{"app-07"}, s.clock)
	s.d = New(Deps{
		Discover:  func(context.Context) ([]fleet.Host, error) { return s.hosts, nil },
		Collector: collect.New(idleKernel{}, s.window, func() time.Time { return s.clock }),
		Detector:  detect.New(detect.DefaultConfig()),
		Planner:   s.planner,
		Actuator:  s.actuator,
		Events:    events.NewWriter(&s.log),
		Metrics:   s.metrics,
		Now:       func() time.Time { return s.clock },
	})
	s.run(59, s.healthy)
	if len(s.actuator.calls) != 0 {
		t.Fatalf("calls = %q, want app-07 kept out for the cooldown", s.actuator.calls)
	}
	s.run(3, s.healthy)
	if !slices.Equal(s.actuator.calls, []string{"restore app-07"}) {
		t.Fatalf("calls = %q", s.actuator.calls)
	}
}
