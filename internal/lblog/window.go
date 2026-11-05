// Package lblog turns HAProxy's request log into per-server latency and error
// statistics, which is the only view of what clients actually experience.
package lblog

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// noResponseMs stands in for a request the server never answered: HAProxy's
// server timeout, the slowest it could have been.
const noResponseMs = 2000

// Req is one request from the log. TrMs is -1 if the server never responded.
type Req struct {
	Server string
	TrMs   int
	Status int
	At     time.Time
}

// ParseLine parses "... gw <server> <Tr> <ST>", the format set by HAProxy's
// log-format. The fields are read from the end because the syslog header in
// front of them varies. Requests that never reached a server are rejected.
func ParseLine(line string, at time.Time) (Req, bool) {
	fields := strings.Fields(line)
	n := len(fields)
	if n < 4 || fields[n-4] != "gw" {
		return Req{}, false
	}
	server := fields[n-3]
	if server == "<NOSRV>" {
		return Req{}, false
	}
	tr, err := strconv.Atoi(fields[n-2])
	if err != nil {
		return Req{}, false
	}
	status, err := strconv.Atoi(fields[n-1])
	if err != nil {
		return Req{}, false
	}
	return Req{Server: server, TrMs: tr, Status: status, At: at}, true
}

// Window keeps recent requests for each server. Entries must arrive in time
// order, which holds because the listener stamps them on receipt.
type Window struct {
	retain time.Duration

	mu      sync.Mutex
	servers map[string][]Req
}

func NewWindow(retain time.Duration) *Window {
	return &Window{retain: retain, servers: map[string][]Req{}}
}

func (w *Window) Add(r Req) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.servers[r.Server] = append(w.servers[r.Server], r)
	w.prune(r.Server, r.At)
}

// prune drops entries older than retain. The caller holds mu.
func (w *Window) prune(server string, now time.Time) {
	reqs := w.servers[server]
	cutoff := now.Add(-w.retain)
	i := 0
	for i < len(reqs) && reqs[i].At.Before(cutoff) {
		i++
	}
	w.servers[server] = reqs[i:]
}

// Stats summarises the requests of the last span. A request with no response
// counts as an error and as noResponseMs, whatever status it logged.
func (w *Window) Stats(server string, now time.Time, span time.Duration) (p90ms float64, errRate float64, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Pruning here too matters for a drained host, which gets no more Adds.
	w.prune(server, now)

	cutoff := now.Add(-span)
	var latencies []int
	errs := 0
	for _, r := range w.servers[server] {
		if r.At.Before(cutoff) {
			continue
		}
		ms := r.TrMs
		noResponse := ms < 0
		if noResponse {
			ms = noResponseMs
		}
		latencies = append(latencies, ms)
		if noResponse || r.Status >= 500 || r.Status <= 0 {
			errs++
		}
	}
	n = len(latencies)
	if n == 0 {
		return 0, 0, 0
	}
	slices.Sort(latencies)
	rank := (9*n + 9) / 10 // nearest rank: ceil(0.9*n)
	return float64(latencies[rank-1]), float64(errs) / float64(n), n
}

// held reports how many entries are in memory for a server.
func (w *Window) held(server string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.servers[server])
}
