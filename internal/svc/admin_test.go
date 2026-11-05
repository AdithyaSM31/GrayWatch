package svc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func post(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func newTestAdmin(t *testing.T, pool Pool) (http.Handler, *Burner, *MemHog) {
	t.Helper()
	burner, hog := &Burner{}, &MemHog{}
	t.Cleanup(func() {
		burner.Set(0)
		hog.Set(0)
	})
	return NewAdmin(pool, burner, hog), burner, hog
}

func waitActive(t *testing.T, b *Burner, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for b.Active() != want {
		if time.Now().After(deadline) {
			t.Fatalf("Active() = %d after 1s, want %d", b.Active(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLeakHoldsAndReleases(t *testing.T) {
	pool := newFakePool(10)
	h, _, _ := newTestAdmin(t, pool)

	if rec := post(h, "/admin/leak?n=9"); rec.Code != http.StatusOK {
		t.Fatalf("leak n=9: status = %d, want 200", rec.Code)
	}
	if got := pool.Held(); got != 9 {
		t.Fatalf("Held() = %d, want 9", got)
	}

	if rec := post(h, "/admin/leak?n=0"); rec.Code != http.StatusOK {
		t.Fatalf("leak n=0: status = %d, want 200", rec.Code)
	}
	if got := pool.Held(); got != 0 {
		t.Fatalf("Held() = %d, want 0", got)
	}
}

func TestAdminBodyReportsCurrentState(t *testing.T) {
	h, _, _ := newTestAdmin(t, newFakePool(10))
	post(h, "/admin/leak?n=4")
	post(h, "/admin/burn?threads=1")
	rec := post(h, "/admin/memhog?mb=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var state struct {
		Held        int `json:"held"`
		BurnThreads int `json:"burn_threads"`
		MemHogMB    int `json:"memhog_mb"`
		MemHogGoal  int `json:"memhog_goal_mb"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if state.Held != 4 || state.BurnThreads != 1 || state.MemHogGoal != 2 {
		t.Fatalf("state = %+v, want held 4, burn 1, memhog goal 2", state)
	}
}

// The memory fault stalls its own allocation on purpose, so the hook must answer
// without waiting for it.
func TestAdminMemHogAnswersWhileAllocationStalls(t *testing.T) {
	stall := make(chan struct{})
	burner, hog := &Burner{}, &MemHog{alloc: func() []byte { <-stall; return nil }}
	t.Cleanup(func() { close(stall); hog.Set(0) })
	h := NewAdmin(newFakePool(10), burner, hog)

	done := make(chan int, 1)
	go func() { done <- post(h, "/admin/memhog?mb=300").Code }()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("hook still waiting after 1s while the allocation stalls")
	}
}

func TestAdminRejectsBadParams(t *testing.T) {
	h, _, _ := newTestAdmin(t, newFakePool(10))
	for _, path := range []string{
		"/admin/leak", "/admin/leak?n=x", "/admin/leak?n=-1",
		"/admin/burn?threads=x", "/admin/burn?threads=-1",
		"/admin/memhog?mb=x", "/admin/memhog?mb=-1",
	} {
		if rec := post(h, path); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestAdminLeakReportsHoldError(t *testing.T) {
	h, _, _ := newTestAdmin(t, newFakePool(10))
	if rec := post(h, "/admin/leak?n=11"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAdminRoutesArePostOnly(t *testing.T) {
	h, _, _ := newTestAdmin(t, newFakePool(10))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/leak?n=1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestBurnerSetAndStop(t *testing.T) {
	b := &Burner{}
	t.Cleanup(func() { b.Set(0) })
	b.Set(2)
	if got := b.Active(); got != 2 {
		t.Fatalf("Active() = %d, want 2", got)
	}
	b.Set(0)
	waitActive(t, b, 0)
}

func TestBurnerResizes(t *testing.T) {
	b := &Burner{}
	t.Cleanup(func() { b.Set(0) })
	b.Set(3)
	b.Set(1)
	waitActive(t, b, 1)
	b.Set(2)
	waitActive(t, b, 2)
}

func waitHeld(t *testing.T, m *MemHog, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.Held() != want {
		if time.Now().After(deadline) {
			t.Fatalf("Held() = %d after 5s, want %d", m.Held(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMemHogHoldsAndReleases(t *testing.T) {
	m := &MemHog{}
	m.Set(8)
	waitHeld(t, m, 8)
	m.Set(3)
	if got := m.Held(); got != 3 {
		t.Fatalf("Held() = %d right after shrinking, want 3", got)
	}
	m.Set(0)
	if got := m.Held(); got != 0 {
		t.Fatalf("Held() = %d right after release, want 0", got)
	}
}

func TestMemHogRegrowsAfterRelease(t *testing.T) {
	m := &MemHog{}
	t.Cleanup(func() { m.Set(0) })
	m.Set(2)
	waitHeld(t, m, 2)
	m.Set(0)
	m.Set(4)
	waitHeld(t, m, 4)
}

// Reverting the fault must work while the kernel is throttling the allocation,
// and a chunk that finishes after the release must not be kept.
func TestMemHogReleaseWorksWhileAllocationStalls(t *testing.T) {
	stall := make(chan struct{})
	entered := make(chan struct{}, 1)
	m := &MemHog{alloc: func() []byte {
		entered <- struct{}{}
		<-stall
		return make([]byte, 1)
	}}
	m.Set(5)
	<-entered

	released := make(chan struct{})
	go func() { m.Set(0); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("Set(0) still blocked after 1s while the allocation stalls")
	}
	if got := m.Goal(); got != 0 {
		t.Fatalf("Goal() = %d, want 0", got)
	}

	close(stall)
	deadline := time.Now().Add(time.Second)
	for m.growing() {
		if time.Now().After(deadline) {
			t.Fatal("grower still running 1s after the stall ended")
		}
		time.Sleep(time.Millisecond)
	}
	if got := m.Held(); got != 0 {
		t.Fatalf("Held() = %d, want the late chunk dropped", got)
	}
}

func TestMemHogRunsOneGrowerAtATime(t *testing.T) {
	var running, peak atomic.Int64
	m := &MemHog{alloc: func() []byte {
		now := running.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		return make([]byte, 1)
	}}
	t.Cleanup(func() { m.Set(0) })
	for i := 1; i <= 5; i++ {
		m.Set(i * 2)
	}
	waitHeld(t, m, 10)
	if got := peak.Load(); got != 1 {
		t.Fatalf("%d allocations ran at once, want 1", got)
	}
}
