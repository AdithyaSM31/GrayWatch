package loadgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func newServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// runFor runs the generator for d and returns every result once Run has closed
// the channel, so it also proves that the channel gets closed.
func runFor(t *testing.T, cfg Config, d time.Duration) []Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out := make(chan Result)
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, cfg, out) }()
	var rs []Result
	for r := range out {
		rs = append(rs, r)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rs
}

func latencies(rs []Result) []time.Duration {
	ds := make([]time.Duration, len(rs))
	for i, r := range rs {
		ds[i] = r.Latency
	}
	return ds
}

func ok(w http.ResponseWriter, r *http.Request) {}

func TestRunHitsTargetRate(t *testing.T) {
	rs := runFor(t, Config{URL: newServer(t, ok), RPS: 200, Timeout: time.Second}, 2*time.Second)
	if len(rs) < 380 || len(rs) > 420 {
		t.Fatalf("got %d results, want 380..420", len(rs))
	}
}

func TestScheduledTimesFollowRate(t *testing.T) {
	rs := runFor(t, Config{URL: newServer(t, ok), RPS: 100, Timeout: time.Second}, 500*time.Millisecond)
	if len(rs) < 2 {
		t.Fatalf("got %d results, want at least 2", len(rs))
	}
	slices.SortFunc(rs, func(a, b Result) int { return a.Scheduled.Compare(b.Scheduled) })
	for i := 1; i < len(rs); i++ {
		if gap := rs[i].Scheduled.Sub(rs[i-1].Scheduled); gap != 10*time.Millisecond {
			t.Fatalf("gap %d = %v, want exactly 10ms", i, gap)
		}
	}
}

func TestResultRecordsStatusAndServedBy(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "app-07")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	rs := runFor(t, Config{URL: url, RPS: 50, Timeout: time.Second}, 100*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	r := rs[0]
	if r.Status != http.StatusServiceUnavailable || r.ServedBy != "app-07" || r.Err != "" {
		t.Fatalf("result = %+v, want status 503, served by app-07, no error", r)
	}
}

func TestTimeoutRecordedAsError(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	})
	rs := runFor(t, Config{URL: url, RPS: 10, Timeout: 100 * time.Millisecond}, 150*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	for _, r := range rs {
		if r.Status != 0 || r.Err == "" || r.Latency < 100*time.Millisecond {
			t.Fatalf("result = %+v, want status 0, an error, latency >= 100ms", r)
		}
	}
}

func TestConnectionErrorRecordedAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(ok))
	url := srv.URL
	srv.Close()
	rs := runFor(t, Config{URL: url, RPS: 50, Timeout: time.Second}, 100*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	if rs[0].Status != 0 || rs[0].Err == "" {
		t.Fatalf("result = %+v, want status 0 and an error", rs[0])
	}
}

// A server that handles one request at a time takes 50 ms each while requests
// are due every 10 ms. A closed-loop client would slow down and report about
// 50 ms; counting from the scheduled time reports the growing queue instead.
func TestLatencyFromScheduledTime(t *testing.T) {
	var mu sync.Mutex
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	})
	rs := runFor(t, Config{URL: url, RPS: 100, Timeout: 30 * time.Second}, time.Second)
	if len(rs) < 90 || len(rs) > 110 {
		t.Fatalf("got %d results, want 90..110: the generator must not wait for the server", len(rs))
	}
	if p99 := Percentile(latencies(rs), 0.99); p99 <= 500*time.Millisecond {
		t.Fatalf("p99 = %v, want > 500ms", p99)
	}
}

// Stopping the generator must not cancel what is already on the wire, or every
// run would end with a burst of false errors.
func TestInFlightRequestsFinishAfterCancel(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	})
	rs := runFor(t, Config{URL: url, RPS: 20, Timeout: 5 * time.Second}, 100*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	for _, r := range rs {
		if r.Status != http.StatusOK || r.Err != "" {
			t.Fatalf("result = %+v, want status 200 and no error", r)
		}
	}
}

func TestRunRejectsBadRate(t *testing.T) {
	out := make(chan Result)
	if err := Run(context.Background(), Config{URL: "http://127.0.0.1:1/", RPS: 0, Timeout: time.Second}, out); err == nil {
		t.Fatal("Run with RPS 0 succeeded, want error")
	}
	if _, open := <-out; open {
		t.Fatal("out still open after Run returned")
	}
}
