package loadgen

import (
	"encoding/csv"
	"io"
	"math"
	"slices"
	"strconv"
	"time"
)

// rankEpsilon absorbs floating point error in p*n, so that 0.07*100, which is
// 7.000000000000001, gives rank 7 rather than 8.
const rankEpsilon = 1e-9

// Percentile returns the nearest-rank percentile of ds, with p between 0 and 1.
// It returns 0 for empty input.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(ds))
	rank := int(math.Ceil(p*float64(len(sorted)) - rankEpsilon))
	return sorted[max(rank-1, 0)]
}

type Summary struct {
	N        int
	P50, P99 time.Duration
	ErrRate  float64
}

// Summarize counts every request towards latency, including failed ones. An
// error is a 5xx status or a request that got no response.
func Summarize(rs []Result) Summary {
	if len(rs) == 0 {
		return Summary{}
	}
	ds := make([]time.Duration, len(rs))
	errs := 0
	for i, r := range rs {
		ds[i] = r.Latency
		if r.Status >= 500 || r.Status <= 0 {
			errs++
		}
	}
	return Summary{
		N:       len(rs),
		P50:     Percentile(ds, 0.5),
		P99:     Percentile(ds, 0.99),
		ErrRate: float64(errs) / float64(len(rs)),
	}
}

func WriteCSV(w io.Writer, rs []Result) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"scheduled_unix_ns", "latency_us", "status", "served_by", "err"}); err != nil {
		return err
	}
	for _, r := range rs {
		err := cw.Write([]string{
			strconv.FormatInt(r.Scheduled.UnixNano(), 10),
			strconv.FormatInt(r.Latency.Microseconds(), 10),
			strconv.Itoa(r.Status),
			r.ServedBy,
			r.Err,
		})
		if err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
