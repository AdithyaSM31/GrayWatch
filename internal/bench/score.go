package bench

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

// HCObs is one reading of a server's health check state from HAProxy.
type HCObs struct {
	At     time.Time
	Server string
	Down   bool
}

// TrialResult is one fault injected once, and what each side did about it.
type TrialResult struct {
	Scenario, Mode, Health, Target string
	Trial                          int
	HCDetected                     bool
	HCTTD                          time.Duration
	GWDetected                     bool
	GWTTD                          time.Duration
	GWCategory                     string
	CategoryOK                     bool
	FalsePosHosts, Drains          int
	FleetAlert                     bool
	P50, P99                       time.Duration
	ErrRate                        float64
	BaselineP99                    time.Duration
}

func within(at, from, to time.Time) bool {
	return !at.Before(from) && !at.After(to)
}

// Score works out one trial. The fault ran from t0 to t1, and traffic before t0
// from tBase is the healthy reference.
//
// A dep fault has no host to catch: every host slows down together, so the right
// answer is a fleet alert, and any drain at all is the wrong one. Its detection
// therefore reads the first fleet alert, and any server's health check counts.
//
// Dry-run drains count as drains, so detect-only rows show what would have been
// done. Mode says which they were.
func Score(sc Scenario, target string, tBase, t0, t1 time.Time, evs []events.Event, hc []HCObs, reqs []loadgen.Result) TrialResult {
	r := TrialResult{Scenario: sc.Name, Target: target}
	dep := sc.Target == TargetDep
	end := t1.Add(sc.Cooldown)

	var first *events.Event
	falsePos := map[string]bool{}
	for i := range evs {
		e := &evs[i]
		switch e.Type {
		case events.Suspect:
			if !dep && e.Host == target && within(e.TS, t0, t1) && (first == nil || e.TS.Before(first.TS)) {
				first = e
			}
			if within(e.TS, tBase, end) && (e.Host != target || e.TS.Before(t0)) {
				falsePos[e.Host] = true
			}
		case events.FleetAlert:
			if within(e.TS, t0, end) {
				r.FleetAlert = true
			}
			if dep && within(e.TS, t0, t1) && (first == nil || e.TS.Before(first.TS)) {
				first = e
			}
		case events.Drain:
			if within(e.TS, t0, end) && (dep || e.Host == target) {
				r.Drains++
			}
		}
	}
	r.FalsePosHosts = len(falsePos)
	if first != nil {
		r.GWDetected, r.GWTTD = true, first.TS.Sub(t0)
		r.GWCategory = first.Category
		if dep {
			r.GWCategory = "fleet"
		}
		r.CategoryOK = r.GWCategory == sc.ExpectedCategory
	}

	for _, o := range hc {
		if !o.Down || (!dep && o.Server != target) || !within(o.At, t0, t1) {
			continue
		}
		if ttd := o.At.Sub(t0); !r.HCDetected || ttd < r.HCTTD {
			r.HCDetected, r.HCTTD = true, ttd
		}
	}

	var during, before []loadgen.Result
	for _, q := range reqs {
		switch {
		case within(q.Scheduled, t0, t1):
			during = append(during, q)
		case within(q.Scheduled, tBase, t0):
			before = append(before, q)
		}
	}
	s := loadgen.Summarize(during)
	r.P50, r.P99, r.ErrRate = s.P50, s.P99, s.ErrRate
	r.BaselineP99 = loadgen.Summarize(before).P99
	return r
}

var trialColumns = []string{
	"scenario", "mode", "health", "target", "trial",
	"hc_detected", "hc_ttd_ms", "gw_detected", "gw_ttd_ms", "gw_category", "category_ok",
	"false_pos_hosts", "drains", "fleet_alert", "p50_ms", "p99_ms", "err_rate", "baseline_p99_ms",
}

// WriteTrialsCSV writes durations in whole milliseconds.
func WriteTrialsCSV(w io.Writer, rs []TrialResult) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(trialColumns); err != nil {
		return err
	}
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
	for _, r := range rs {
		err := cw.Write([]string{
			r.Scenario, r.Mode, r.Health, r.Target, strconv.Itoa(r.Trial),
			strconv.FormatBool(r.HCDetected), ms(r.HCTTD),
			strconv.FormatBool(r.GWDetected), ms(r.GWTTD), r.GWCategory, strconv.FormatBool(r.CategoryOK),
			strconv.Itoa(r.FalsePosHosts), strconv.Itoa(r.Drains), strconv.FormatBool(r.FleetAlert),
			ms(r.P50), ms(r.P99), strconv.FormatFloat(r.ErrRate, 'f', -1, 64), ms(r.BaselineP99),
		})
		if err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// CheckOutput refuses to start a run that would replace finished trials, as a run
// saves from its first moment and a full benchmark takes hours to repeat.
func CheckOutput(path string, force bool) error {
	if force {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Count(string(data), "\n") > 1 {
		return fmt.Errorf("%s already has results: choose another -out, or pass -force to replace them", path)
	}
	return nil
}

// SaveTrialsCSV replaces path in one step, so a crash mid-write cannot lose the
// trials already finished.
func SaveTrialsCSV(path string, rs []TrialResult) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := WriteTrialsCSV(f, rs); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
