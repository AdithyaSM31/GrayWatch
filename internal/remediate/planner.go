// Package remediate decides when to take hosts out of the load balancer and when
// to put them back, within limits that keep it from making an outage worse.
package remediate

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/detect"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
)

// capEpsilon stops floating point error from losing a host in the drain cap:
// 0.29*100 is 28.999999999999996.
const capEpsilon = 1e-9

type Policy struct {
	MaxDrainFrac, FleetWideFrac        float64
	Cooldown, MinActionGap, FlapWindow time.Duration
	MaxDrainsPerWindow                 int
	DryRun                             bool
}

func DefaultPolicy() Policy {
	return Policy{
		MaxDrainFrac:       0.10,
		FleetWideFrac:      0.30,
		Cooldown:           60 * time.Second,
		MinActionGap:       10 * time.Second,
		FlapWindow:         10 * time.Minute,
		MaxDrainsPerWindow: 3,
	}
}

type ActionKind string

const (
	Drain           ActionKind = "drain"
	Restore         ActionKind = "restore"
	AlertFleetWide  ActionKind = "fleet_wide"
	AlertCapReached ActionKind = "cap_reached"
	AlertFlapping   ActionKind = "flapping"
)

type Action struct {
	Kind         ActionKind
	Host, Reason string
	DryRun       bool
}

// Planner is split into Plan, which only proposes, and Commit, which records what
// really happened. A failed drain therefore never counts as drained.
type Planner struct {
	policy      Policy
	fleetSize   int
	drained     map[string]time.Time
	history     map[string][]time.Time
	flapAlerted map[string]bool
	lastAction  time.Time
	retryAt     map[string]time.Time // when a failed restore may be tried again
	othersOut   map[string]bool      // out of rotation, but not drained by the planner
	down        map[string]bool      // failing HAProxy's health checks
}

// NewPlanner starts with the servers HAProxy already has out of rotation, which
// count toward the cap. Their cooldown runs from now: after a restart nothing is
// known about them yet, and they must not go straight back while still sick.
func NewPlanner(p Policy, fleetSize int, alreadyDrained []string, now time.Time) *Planner {
	pl := &Planner{
		policy:      p,
		fleetSize:   fleetSize,
		drained:     map[string]time.Time{},
		history:     map[string][]time.Time{},
		flapAlerted: map[string]bool{},
		retryAt:     map[string]time.Time{},
	}
	for _, host := range alreadyDrained {
		pl.drained[host] = now
	}
	return pl
}

// Reconcile updates the planner from what HAProxy reports. A server back in
// rotation is no longer drained, which covers HAProxy restarting and a person
// setting it back. Servers out for any other reason count against the guards: the
// limits are about how much of the fleet is out, whoever took it out. A server
// taken out by its health check also reads healthy here, because it has no traffic.
func (pl *Planner) Reconcile(stats []haproxy.ServerStat) {
	pl.othersOut = map[string]bool{}
	pl.down = map[string]bool{}
	for _, s := range stats {
		out := strings.HasPrefix(s.Status, "DRAIN") || strings.HasPrefix(s.Status, "MAINT") || s.Down()
		if s.Down() {
			pl.down[s.Name] = true
		}
		if _, ours := pl.drained[s.Name]; ours {
			if !out {
				delete(pl.drained, s.Name)
				delete(pl.retryAt, s.Name)
			}
			continue
		}
		if out {
			pl.othersOut[s.Name] = true
		}
	}
}

func (pl *Planner) cap() int {
	return max(1, int(math.Floor(pl.policy.MaxDrainFrac*float64(pl.fleetSize)+capEpsilon)))
}

// Plan proposes actions for one tick and changes nothing. It allows at most one
// Drain or Restore per call, so each one's effect can be seen before the next.
func (pl *Planner) Plan(obs detect.Observation) []Action {
	now := obs.At
	verdicts := make(map[string]detect.Verdict, len(obs.Verdicts))
	sick := maps.Clone(pl.down)
	if sick == nil {
		sick = map[string]bool{}
	}
	for _, v := range obs.Verdicts {
		verdicts[v.Host] = v
		if v.State == detect.Suspect {
			sick[v.Host] = true
		}
	}

	if obs.Fleet != nil {
		return []Action{pl.action(AlertFleetWide, "", obs.Fleet.Reason)}
	}
	if float64(len(sick)) > pl.policy.FleetWideFrac*float64(pl.fleetSize) {
		return []Action{pl.action(AlertFleetWide, "", fmt.Sprintf("%d of %d hosts are suspect or down", len(sick), pl.fleetSize))}
	}
	if !pl.lastAction.IsZero() && now.Sub(pl.lastAction) < pl.policy.MinActionGap {
		return nil
	}

	var actions []Action
	for _, host := range slices.Sorted(maps.Keys(pl.drained)) {
		// A host with no verdict reads as neither state, so it is not restored.
		if verdicts[host].State != detect.Healthy || now.Sub(pl.drained[host]) < pl.policy.Cooldown || now.Before(pl.retryAt[host]) {
			continue
		}
		if drains := pl.recentDrains(host, now); drains >= pl.policy.MaxDrainsPerWindow {
			if !pl.flapAlerted[host] {
				reason := fmt.Sprintf("drained %d times in %v", drains, pl.policy.FlapWindow)
				actions = append(actions, pl.action(AlertFlapping, host, reason))
			}
			continue
		}
		actions = append(actions, pl.action(Restore, host, fmt.Sprintf("healthy for the %v cooldown", pl.policy.Cooldown)))
		// A dry run changes nothing, so this restore comes back every tick. It must
		// not hide the drains behind it.
		if !pl.policy.DryRun {
			return actions
		}
	}

	target, ok := pl.worstSuspect(obs)
	if !ok {
		return actions
	}
	if out := len(pl.drained) + len(pl.othersOut); out >= pl.cap() {
		reason := fmt.Sprintf("%d of %d hosts already out of rotation", out, pl.fleetSize)
		return append(actions, pl.action(AlertCapReached, target.Host, reason))
	}
	return append(actions, pl.action(Drain, target.Host, fmt.Sprintf("suspect with score %.1f", target.Score)))
}

func (pl *Planner) action(kind ActionKind, host, reason string) Action {
	return Action{Kind: kind, Host: host, Reason: reason, DryRun: pl.policy.DryRun}
}

// worstSuspect picks the highest score. Verdicts are sorted by host, so a tie goes
// to the first by name.
func (pl *Planner) worstSuspect(obs detect.Observation) (detect.Verdict, bool) {
	var worst detect.Verdict
	found := false
	for _, v := range obs.Verdicts {
		if v.State != detect.Suspect {
			continue
		}
		if _, drained := pl.drained[v.Host]; drained {
			continue
		}
		if !found || v.Score > worst.Score {
			worst, found = v, true
		}
	}
	return worst, found
}

func (pl *Planner) recentDrains(host string, now time.Time) int {
	n := 0
	for _, at := range pl.history[host] {
		if now.Sub(at) < pl.policy.FlapWindow {
			n++
		}
	}
	return n
}

// Commit records the outcome of an action. A failed action changes nothing, and
// neither does a dry run. A failed drain is retried on the next tick with no gap.
// A failed restore waits, so that it can't hide drains behind it every tick.
func (pl *Planner) Commit(a Action, at time.Time, err error) {
	if err != nil && a.Kind == Restore {
		pl.retryAt[a.Host] = at.Add(pl.policy.MinActionGap)
	}
	if err != nil || a.DryRun {
		return
	}
	switch a.Kind {
	case Drain:
		pl.drained[a.Host] = at
		pl.history[a.Host] = append(pl.history[a.Host], at)
		pl.lastAction = at
	case Restore:
		delete(pl.drained, a.Host)
		delete(pl.flapAlerted, a.Host)
		delete(pl.retryAt, a.Host)
		pl.lastAction = at
	case AlertFlapping:
		pl.flapAlerted[a.Host] = true
	}
}

// Drained returns a copy of the hosts out of rotation.
func (pl *Planner) Drained() map[string]bool {
	out := make(map[string]bool, len(pl.drained))
	for host := range pl.drained {
		out[host] = true
	}
	return out
}
