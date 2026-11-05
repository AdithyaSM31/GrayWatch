// Package loadgen is an open-loop load generator: requests go out on a fixed
// schedule whether or not earlier ones have finished, and latency counts from
// the scheduled time, so a slow server cannot hide by slowing the client down.
package loadgen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// idleConnsPerHost keeps connections reusable at high rates. Go's default of 2
// would open a new connection for nearly every request and exhaust ports.
const idleConnsPerHost = 1000

type Result struct {
	Scheduled time.Time
	Latency   time.Duration
	Status    int // 0 on a transport error or timeout
	ServedBy  string
	Err       string
}

type Config struct {
	URL     string
	RPS     int
	Timeout time.Duration
}

// Run sends requests until ctx ends, then waits for those in flight and closes
// out. In-flight requests are bounded by cfg.Timeout, not by ctx, because
// cancelling them would record false errors at the end of every run.
func Run(ctx context.Context, cfg Config, out chan<- Result) error {
	defer close(out)
	if cfg.RPS <= 0 {
		return errors.New("RPS must be positive")
	}

	transport := &http.Transport{MaxIdleConnsPerHost: idleConnsPerHost, IdleConnTimeout: 90 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: cfg.Timeout, Transport: transport}

	var wg sync.WaitGroup
	defer wg.Wait()

	start := time.Now()
	for k := 0; ; k++ {
		sched := start.Add(time.Duration(k) * time.Second / time.Duration(cfg.RPS))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(sched)):
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out <- fire(client, cfg.URL, sched)
		}()
	}
}

func fire(client *http.Client, url string, sched time.Time) Result {
	r := Result{Scheduled: sched}
	resp, err := client.Get(url)
	if err != nil {
		r.Err = err.Error()
	} else {
		defer resp.Body.Close()
		r.ServedBy = resp.Header.Get("X-Served-By")
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			r.Err = err.Error()
		} else {
			r.Status = resp.StatusCode
		}
	}
	r.Latency = time.Since(sched)
	return r
}
