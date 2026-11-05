package loadgen

import (
	"bytes"
	"slices"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	ds := make([]time.Duration, 100)
	for i := range ds {
		ds[i] = time.Duration(i+1) * time.Millisecond
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{
		{0.99, 99 * time.Millisecond},
		{0.5, 50 * time.Millisecond},
		{1, 100 * time.Millisecond},
		{0, 1 * time.Millisecond},
		// 0.07*100 is 7.000000000000001 in floating point, which must not
		// round up to rank 8.
		{0.07, 7 * time.Millisecond},
	} {
		if got := Percentile(ds, tc.p); got != tc.want {
			t.Errorf("Percentile(1..100ms, %v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := Percentile(nil, 0.5); got != 0 {
		t.Errorf("Percentile(empty) = %v, want 0", got)
	}
}

func TestPercentileDoesNotReorderInput(t *testing.T) {
	ds := []time.Duration{3, 1, 2}
	if got := Percentile(ds, 1); got != 3 {
		t.Fatalf("Percentile = %v, want 3", got)
	}
	if !slices.Equal(ds, []time.Duration{3, 1, 2}) {
		t.Fatalf("input reordered to %v", ds)
	}
}

func TestSummarize(t *testing.T) {
	rs := []Result{
		{Latency: 10 * time.Millisecond, Status: 200},
		{Latency: 20 * time.Millisecond, Status: 200},
		{Latency: 30 * time.Millisecond, Status: 503},
		{Latency: 40 * time.Millisecond, Status: 0, Err: "timeout"},
	}
	s := Summarize(rs)
	if s.N != 4 || s.ErrRate != 0.5 {
		t.Errorf("N/ErrRate = %d/%v, want 4/0.5", s.N, s.ErrRate)
	}
	// Failed requests count towards latency: a timeout is the slowest kind of
	// request, and dropping it would hide exactly what this tool measures.
	if s.P50 != 20*time.Millisecond || s.P99 != 40*time.Millisecond {
		t.Errorf("P50/P99 = %v/%v, want 20ms/40ms", s.P50, s.P99)
	}
	if empty := Summarize(nil); empty.N != 0 || empty.ErrRate != 0 {
		t.Errorf("Summarize(nil) = %+v, want zero", empty)
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	err := WriteCSV(&buf, []Result{
		{Scheduled: time.Unix(0, 1500), Latency: 2500 * time.Microsecond, Status: 200, ServedBy: "app-07"},
		{Scheduled: time.Unix(0, 2500), Latency: 2 * time.Second, Err: "dial: refused, try later"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "scheduled_unix_ns,latency_us,status,served_by,err\n" +
		"1500,2500,200,app-07,\n" +
		"2500,2000000,0,,\"dial: refused, try later\"\n"
	if got := buf.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
