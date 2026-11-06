package bench

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const noneScenario = "none"

// Row is one fault's results across the configs.
//
// The health check columns need care. HC caught (shallow) reads shallow:detect rows
// only: in a remediate run Graywatch drains first, and a server in MAINT cannot read
// DOWN, so those rows say nothing about what the health check would have done.
type Row struct {
	Fault                                     string
	HCShallow, HCDeep, Graywatch, Category    string // each k/n
	FleetAlerts                               string // k/n
	MedianTTD                                 time.Duration
	FalsePosHosts                             int
	P99Detect, P99Remediate                   time.Duration
	ErrDetect, ErrRemediate, ErrDeep          float64
	detectTrials, remediateTrials, deepTrials int
}

func rate(rs []TrialResult, hit func(TrialResult) bool) string {
	if len(rs) == 0 {
		return "n/a"
	}
	k := 0
	for _, r := range rs {
		if hit(r) {
			k++
		}
	}
	return fmt.Sprintf("%d/%d", k, len(rs))
}

func median[T time.Duration | float64](xs []T) T {
	if len(xs) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(xs))
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func collect[T any](rs []TrialResult, get func(TrialResult) T) []T {
	out := make([]T, len(rs))
	for i, r := range rs {
		out[i] = get(r)
	}
	return out
}

func filter(rs []TrialResult, keep func(TrialResult) bool) []TrialResult {
	var out []TrialResult
	for _, r := range rs {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// Summarize gives one row per fault, sorted by name, leaving out the no-fault
// baseline. Detect-only means shallow:detect: with deep checks, the health check
// takes the host out itself, so those rows are shown separately.
func Summarize(rs []TrialResult) []Row {
	byFault := map[string][]TrialResult{}
	for _, r := range rs {
		if r.Scenario != noneScenario {
			byFault[r.Scenario] = append(byFault[r.Scenario], r)
		}
	}
	var rows []Row
	for _, fault := range slices.Sorted(maps.Keys(byFault)) {
		all := byFault[fault]
		detect := filter(all, func(r TrialResult) bool { return r.Health == "shallow" && r.Mode == "detect" })
		remediate := filter(all, func(r TrialResult) bool { return r.Mode == "remediate" })
		deep := filter(all, func(r TrialResult) bool { return r.Health == "deep" && r.Mode == "detect" })
		detected := filter(all, func(r TrialResult) bool { return r.GWDetected })

		falsePos := 0
		for _, r := range all {
			falsePos += r.FalsePosHosts
		}
		category := rate(all, func(r TrialResult) bool { return r.CategoryOK })
		// A fleet alert is what catching this fault means, so the score can't fail.
		if len(detected) > 0 && !slices.ContainsFunc(detected, func(r TrialResult) bool { return r.GWCategory != "fleet" }) {
			category = "n/a"
		}
		hc := func(r TrialResult) bool { return r.HCDetected }
		p99 := func(r TrialResult) time.Duration { return r.P99 }
		errs := func(r TrialResult) float64 { return r.ErrRate }
		rows = append(rows, Row{
			Fault:           fault,
			HCShallow:       rate(detect, hc),
			HCDeep:          rate(deep, hc),
			Graywatch:       rate(all, func(r TrialResult) bool { return r.GWDetected }),
			Category:        category,
			FleetAlerts:     rate(all, func(r TrialResult) bool { return r.FleetAlert }),
			MedianTTD:       median(collect(detected, func(r TrialResult) time.Duration { return r.GWTTD })),
			FalsePosHosts:   falsePos,
			P99Detect:       median(collect(detect, p99)),
			P99Remediate:    median(collect(remediate, p99)),
			ErrDetect:       median(collect(detect, errs)),
			ErrRemediate:    median(collect(remediate, errs)),
			ErrDeep:         median(collect(deep, errs)),
			detectTrials:    len(detect),
			remediateTrials: len(remediate),
			deepTrials:      len(deep),
		})
	}
	return rows
}

// RenderMarkdown writes the plan's results table, an error table, and the rate of
// false positives with no fault running.
func RenderMarkdown(rows []Row, fpPerHostHour float64) string {
	ms := func(d time.Duration, n int) string {
		if n == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	pct := func(x float64, n int) string {
		if n == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.2f%%", x*100)
	}

	var b strings.Builder
	b.WriteString("| Fault | HC caught (shallow) | HC caught (deep) | Graywatch caught | Median TTD | Category right | False-positive hosts | p99 detect-only | p99 remediate |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		ttd := "n/a"
		if !strings.HasPrefix(r.Graywatch, "0/") {
			ttd = fmt.Sprintf("%.1f s", r.MedianTTD.Seconds())
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %s | %s |\n",
			r.Fault, r.HCShallow, r.HCDeep, r.Graywatch, ttd, r.Category, r.FalsePosHosts,
			ms(r.P99Detect, r.detectTrials), ms(r.P99Remediate, r.remediateTrials))
	}

	fmt.Fprintf(&b, "\nFalse positives with no fault running: %.2f per host-hour.\n", fpPerHostHour)

	b.WriteString("\nFleet-wide request errors during the fault, as the median across trials:\n\n")
	b.WriteString("| Fault | Errors detect-only | Errors remediate | Errors deep health check | Fleet alerts |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Fault,
			pct(r.ErrDetect, r.detectTrials), pct(r.ErrRemediate, r.remediateTrials), pct(r.ErrDeep, r.deepTrials), r.FleetAlerts)
	}
	return b.String()
}

// FalsePositiveRate is hosts flagged per host-hour across the no-fault windows. No
// such window is an error, as a rate of 0 would claim what was never measured.
func FalsePositiveRate(rs []TrialResult, hosts int, window time.Duration) (rate, hostHours float64, err error) {
	flagged := 0
	for _, r := range rs {
		if r.Scenario == noneScenario {
			flagged += r.FalsePosHosts
			hostHours += float64(hosts) * window.Hours()
		}
	}
	if hostHours == 0 {
		return 0, 0, errors.New("no results from the no-fault scenario")
	}
	return float64(flagged) / hostHours, hostHours, nil
}

// Report is the whole of docs/RESULTS.md. Notes are hand-written and come last, as
// what they say depends on the run.
func Report(rs []TrialResult, hosts int, noFaultWindow time.Duration, notes string) (string, error) {
	fp, hostHours, err := FalsePositiveRate(rs, hosts, noFaultWindow)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Results\n\n")
	b.WriteString("Generated by `bench report`. Do not edit this file; hand-written notes come from `notes.md` beside `trials.csv`.\n\n")
	b.WriteString(RenderMarkdown(Summarize(rs), fp))
	fmt.Fprintf(&b, "\nThe no-fault windows cover %.2f host-hours.\n", hostHours)
	if notes = strings.TrimSpace(notes); notes != "" {
		b.WriteString("\n" + notes + "\n")
	}
	return b.String(), nil
}

// ReadTrialsCSV reads what WriteTrialsCSV wrote.
func ReadTrialsCSV(r io.Reader) ([]TrialResult, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("trials file is empty")
	}
	if got := strings.Join(rows[0], ","); got != strings.Join(trialColumns, ",") {
		return nil, fmt.Errorf("unexpected columns: %s", got)
	}
	out := make([]TrialResult, 0, len(rows)-1)
	for i, row := range rows[1:] {
		t, err := parseTrial(row)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func parseTrial(row []string) (TrialResult, error) {
	var errs []error
	integer := func(s string) int {
		n, err := strconv.Atoi(s)
		errs = append(errs, err)
		return n
	}
	boolean := func(s string) bool {
		v, err := strconv.ParseBool(s)
		errs = append(errs, err)
		return v
	}
	ms := func(s string) time.Duration { return time.Duration(integer(s)) * time.Millisecond }
	errRate, err := strconv.ParseFloat(row[16], 64)
	errs = append(errs, err)
	t := TrialResult{
		Scenario: row[0], Mode: row[1], Health: row[2], Target: row[3], Trial: integer(row[4]),
		HCDetected: boolean(row[5]), HCTTD: ms(row[6]),
		GWDetected: boolean(row[7]), GWTTD: ms(row[8]), GWCategory: row[9], CategoryOK: boolean(row[10]),
		FalsePosHosts: integer(row[11]), Drains: integer(row[12]), FleetAlert: boolean(row[13]),
		P50: ms(row[14]), P99: ms(row[15]), ErrRate: errRate, BaselineP99: ms(row[17]),
	}
	return t, errors.Join(errs...)
}
