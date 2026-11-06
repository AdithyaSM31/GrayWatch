package svc

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	mebibyte = 1 << 20
	pageSize = 4 << 10
)

// NewAdmin serves the fault hooks. It listens on a port that is never
// published, and each route returns the service's current fault state.
func NewAdmin(pool Pool, burner *Burner, hog *MemHog) http.Handler {
	mux := http.NewServeMux()
	route := func(path, param string, apply func(int) error) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			n, err := strconv.Atoi(r.URL.Query().Get(param))
			if err != nil || n < 0 {
				http.Error(w, param+" must be a non-negative integer", http.StatusBadRequest)
				return
			}
			if err := apply(n); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]int{
				"held":           pool.Held(),
				"burn_threads":   burner.Active(),
				"memhog_mb":      hog.Held(),
				"memhog_goal_mb": hog.Goal(),
			})
		})
	}
	route("/admin/leak", "n", pool.Hold)
	route("/admin/burn", "threads", func(n int) error { burner.Set(n); return nil })
	route("/admin/memhog", "mb", func(n int) error { hog.Set(n); return nil })
	return mux
}

// Burner spins goroutines to create CPU contention inside the container.
// The zero value is ready to use.
type Burner struct {
	mu     sync.Mutex
	stops  []chan struct{}
	active atomic.Int64
}

func (b *Burner) Set(threads int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.stops) < threads {
		stop := make(chan struct{})
		b.stops = append(b.stops, stop)
		b.active.Add(1)
		go b.spin(stop)
	}
	for len(b.stops) > threads {
		last := len(b.stops) - 1
		close(b.stops[last])
		b.stops = b.stops[:last]
	}
}

func (b *Burner) spin(stop <-chan struct{}) {
	defer b.active.Add(-1)
	for {
		select {
		case <-stop:
			return
		default:
		}
	}
}

// Active counts goroutines still spinning, so it reaches the target of a
// shrinking Set only once they have really stopped.
func (b *Burner) Active() int { return int(b.active.Load()) }

// MemHog holds resident memory to create memory pressure. The zero value is ready
// to use.
//
// Set only records the goal. A background goroutine allocates toward it, because
// the fault works by having memory.high stall these allocations in the kernel, for
// longer than any caller should wait. No lock is held while allocating, so a
// release still works during a stall.
type MemHog struct {
	mu     sync.Mutex
	chunks [][]byte
	goal   int
	busy   bool
	alloc  func() []byte // nil means allocateChunk; tests replace it
}

func (m *MemHog) Set(mb int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.goal = mb
	if len(m.chunks) > mb {
		clear(m.chunks[mb:])
		m.chunks = m.chunks[:mb]
		debug.FreeOSMemory()
	}
	if len(m.chunks) < mb && !m.busy {
		m.busy = true
		go m.grow()
	}
}

func (m *MemHog) grow() {
	alloc := m.alloc
	if alloc == nil {
		alloc = allocateChunk
	}
	for {
		m.mu.Lock()
		if len(m.chunks) >= m.goal {
			m.busy = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		chunk := alloc()

		m.mu.Lock()
		if len(m.chunks) < m.goal { // the goal may have dropped during the stall
			m.chunks = append(m.chunks, chunk)
		}
		m.mu.Unlock()
	}
}

func allocateChunk() []byte {
	chunk := make([]byte, mebibyte)
	for i := 0; i < len(chunk); i += pageSize {
		chunk[i] = 1
	}
	return chunk
}

// Held is how many MiB are really held, which lags Goal while allocating.
func (m *MemHog) Held() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.chunks)
}

// Goal is how many MiB the last Set asked for.
func (m *MemHog) Goal() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.goal
}

func (m *MemHog) growing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy
}
