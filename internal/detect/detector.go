package detect

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/collect"
)

type HostState string

const (
	Healthy HostState = "healthy"
	Suspect HostState = "suspect"
)

// Verdict is one host's state. Signals, Category and Score describe the latest
// tick that flagged a Suspect host, and are empty for a Healthy one.
type Verdict struct {
	Host     string
	State    HostState
	Signals  []collect.Signal
	Category string
	Score    float64
	Since    time.Time
}

// FleetAlert means the whole fleet moved together, which points at something
// shared. Draining hosts would not help and would make things worse.
type FleetAlert struct {
	Reason                 string
	MedianLatMs, MedianErr float64
}

type Observation struct {
	At       time.Time
	Verdicts []Verdict
	Fleet    *FleetAlert
}

type hostHistory struct {
	state    HostState
	since    time.Time
	hits     []bool
	signals  []collect.Signal
	category string
	score    float64
}

// Detector is not safe for concurrent use; the daemon calls it from one loop.
type Detector struct {
	cfg     Config
	signals []collect.Signal
	hosts   map[string]*hostHistory
}

func New(cfg Config) *Detector {
	return &Detector{
		cfg:     cfg,
		signals: slices.Sorted(maps.Keys(cfg.MinMAD)),
		hosts:   map[string]*hostHistory{},
	}
}

type flag struct {
	signal collect.Signal
	z      float64
}

// Observe judges one tick. Drained hosts are left out of the peer baseline and
// the fleet alert, but are still judged against the baseline on whatever signals
// they have, so one that is still sick at the kernel level stays Suspect.
func (d *Detector) Observe(samples []collect.Sample, drained map[string]bool) Observation {
	var obs Observation
	if len(samples) > 0 {
		obs.At = samples[0].At
	}

	flags := d.flagOutliers(samples, drained)
	present := make(map[string]bool, len(samples))
	for _, s := range samples {
		present[s.Host] = true
		obs.Verdicts = append(obs.Verdicts, d.update(s.Host, flags[s.Host], obs.At))
	}
	// A host that left the fleet has a new container if it returns.
	maps.DeleteFunc(d.hosts, func(host string, _ *hostHistory) bool { return !present[host] })
	slices.SortFunc(obs.Verdicts, func(a, b Verdict) int { return strings.Compare(a.Host, b.Host) })

	obs.Fleet = d.fleetAlert(samples, drained)
	return obs
}

func (d *Detector) flagOutliers(samples []collect.Sample, drained map[string]bool) map[string][]flag {
	flags := map[string][]flag{}
	for _, sig := range d.signals {
		peers := peerValues(samples, drained, sig)
		if len(peers) < d.cfg.MinHosts {
			continue
		}
		median, spread := baseline(peers, d.cfg.MinMAD[sig])
		for _, s := range samples {
			x, ok := s.Values[sig]
			if !ok {
				continue
			}
			z := zScore(x, median, spread)
			if z > d.cfg.Z && x-median > d.cfg.MinDelta[sig] {
				flags[s.Host] = append(flags[s.Host], flag{sig, z})
			}
		}
	}
	return flags
}

func peerValues(samples []collect.Sample, drained map[string]bool, sig collect.Signal) []float64 {
	var values []float64
	for _, s := range samples {
		if x, ok := s.Values[sig]; ok && !drained[s.Host] {
			values = append(values, x)
		}
	}
	return values
}

func (d *Detector) update(host string, flagged []flag, now time.Time) Verdict {
	h, ok := d.hosts[host]
	if !ok {
		h = &hostHistory{state: Healthy}
		d.hosts[host] = h
	}

	h.hits = append(h.hits, len(flagged) > 0)
	if len(h.hits) > d.cfg.Window {
		h.hits = h.hits[1:]
	}
	if len(flagged) > 0 {
		h.signals, h.category, h.score = d.describe(flagged)
	}

	hits := 0
	for _, hit := range h.hits {
		if hit {
			hits++
		}
	}
	switch {
	case h.state == Healthy && hits >= d.cfg.Need:
		h.state, h.since = Suspect, now
	case h.state == Suspect && hits == 0:
		h.state, h.since = Healthy, now
	}

	v := Verdict{Host: host, State: h.state, Since: h.since}
	if h.state == Suspect {
		v.Signals, v.Category, v.Score = h.signals, h.category, h.score
	}
	return v
}

// describe summarises a flagged tick. The category comes from the worst kernel
// signal, because latency and errors are symptoms while a kernel signal names
// the cause, however large the symptom's own z-score is.
func (d *Detector) describe(flagged []flag) (signals []collect.Signal, category string, score float64) {
	category = appCategory
	var worstCause float64
	for _, f := range flagged {
		signals = append(signals, f.signal)
		score = max(score, f.z)
		if c := d.cfg.Category[f.signal]; c != appCategory && f.z > worstCause {
			category, worstCause = c, f.z
		}
	}
	return signals, category, score
}

func (d *Detector) fleetAlert(samples []collect.Sample, drained map[string]bool) *FleetAlert {
	var alert FleetAlert
	var reasons []string
	if lat := peerValues(samples, drained, collect.SigLatP90); len(lat) >= d.cfg.MinHosts {
		alert.MedianLatMs = Median(lat)
		if alert.MedianLatMs > d.cfg.FleetLatencyMs {
			reasons = append(reasons, fmt.Sprintf("median latency %.0f ms is above %.0f ms", alert.MedianLatMs, d.cfg.FleetLatencyMs))
		}
	}
	if errs := peerValues(samples, drained, collect.SigErrRate); len(errs) >= d.cfg.MinHosts {
		alert.MedianErr = Median(errs)
		if alert.MedianErr > d.cfg.FleetErrRate {
			reasons = append(reasons, fmt.Sprintf("median error rate %.3f is above %.3f", alert.MedianErr, d.cfg.FleetErrRate))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	alert.Reason = strings.Join(reasons, "; ")
	return &alert
}
