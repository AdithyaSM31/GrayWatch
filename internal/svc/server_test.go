package svc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakePool is a semaphore that behaves like the real pool: at most cap
// concurrent users, and a 1 s acquire timeout inside Query and Ping.
type fakePool struct {
	sem  chan struct{}
	held int
}

func newFakePool(size int) *fakePool {
	return &fakePool{sem: make(chan struct{}, size)}
}

func (p *fakePool) acquire(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	select {
	case p.sem <- struct{}{}:
		<-p.sem
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *fakePool) Query(ctx context.Context) error { return p.acquire(ctx) }
func (p *fakePool) Ping(ctx context.Context) error  { return p.acquire(ctx) }

func (p *fakePool) Hold(n int) error {
	if n < 0 || n > cap(p.sem) {
		return errors.New("hold out of range")
	}
	for p.held < n {
		p.sem <- struct{}{}
		p.held++
	}
	for p.held > n {
		<-p.sem
		p.held--
	}
	return nil
}

func (p *fakePool) Held() int { return p.held }

func newDep(t *testing.T, status int) *httptest.Server {
	t.Helper()
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(dep.Close)
	return dep
}

func closedDep(t *testing.T) *httptest.Server {
	t.Helper()
	dep := newDep(t, http.StatusOK)
	dep.Close()
	return dep
}

// testAuditPath does not use t.TempDir: the server keeps the audit file open,
// and on Windows that would make TempDir's cleanup fail the test.
func testAuditPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "svc-audit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "audit.log")
}

func newTestServer(t *testing.T, mode string, pool Pool, depURL string) http.Handler {
	t.Helper()
	return NewServer(Config{
		Hostname:   "app-07",
		DepURL:     depURL,
		HealthMode: mode,
		AuditPath:  testAuditPath(t),
	}, pool, &http.Client{
		Timeout:   time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	})
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestWorkSetsServedBy(t *testing.T) {
	h := newTestServer(t, "shallow", newFakePool(10), newDep(t, http.StatusOK).URL)
	rec := get(h, "/work")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Served-By"); got != "app-07" {
		t.Fatalf("X-Served-By = %q, want app-07", got)
	}
}

// The chaos scoring attributes failures by this header, so failing and
// unrouted responses must carry it too.
func TestEveryResponseSetsServedBy(t *testing.T) {
	pool := newFakePool(10)
	if err := pool.Hold(10); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, "deep", pool, closedDep(t).URL)
	for _, path := range []string{"/work", "/health", "/nope"} {
		if got := get(h, path).Header().Get("X-Served-By"); got != "app-07" {
			t.Errorf("%s: X-Served-By = %q, want app-07", path, got)
		}
	}
}

func TestWorkReturns500WhenPoolExhausted(t *testing.T) {
	pool := newFakePool(10)
	if err := pool.Hold(10); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, "shallow", pool, newDep(t, http.StatusOK).URL)
	start := time.Now()
	rec := get(h, "/work")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("took %v, want < 1.5s", elapsed)
	}
}

func TestWorkReturns500WhenDepDown(t *testing.T) {
	h := newTestServer(t, "shallow", newFakePool(10), closedDep(t).URL)
	if rec := get(h, "/work"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWorkReturns500WhenDepReturnsError(t *testing.T) {
	h := newTestServer(t, "shallow", newFakePool(10), newDep(t, http.StatusBadGateway).URL)
	if rec := get(h, "/work"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWorkAppendsAuditRecord(t *testing.T) {
	path := testAuditPath(t)
	h := NewServer(Config{Hostname: "app-07", DepURL: newDep(t, http.StatusOK).URL, HealthMode: "shallow", AuditPath: path},
		newFakePool(10), http.DefaultClient)
	for i := 0; i < 3; i++ {
		if rec := get(h, "/work"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 3*4096 {
		t.Fatalf("audit size = %d, want %d", fi.Size(), 3*4096)
	}
}

func TestWorkReturns500WhenAuditFails(t *testing.T) {
	path := filepath.Join(filepath.Dir(testAuditPath(t)), "missing-dir", "audit.log")
	h := NewServer(Config{Hostname: "app-07", DepURL: newDep(t, http.StatusOK).URL, HealthMode: "shallow", AuditPath: path},
		newFakePool(10), http.DefaultClient)
	if rec := get(h, "/work"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWorkBurnsCPUForWorkCPU(t *testing.T) {
	h := NewServer(Config{Hostname: "app-07", DepURL: newDep(t, http.StatusOK).URL, HealthMode: "shallow",
		AuditPath: testAuditPath(t), WorkCPU: 30 * time.Millisecond}, newFakePool(10), http.DefaultClient)
	start := time.Now()
	if rec := get(h, "/work"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("took %v, want at least the 30ms WorkCPU", elapsed)
	}
}

func TestHealthShallowIgnoresDeps(t *testing.T) {
	pool := newFakePool(10)
	if err := pool.Hold(10); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, "shallow", pool, closedDep(t).URL)
	if rec := get(h, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthDeepOKWhenDepsHealthy(t *testing.T) {
	h := newTestServer(t, "deep", newFakePool(10), newDep(t, http.StatusOK).URL)
	if rec := get(h, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthDeepFailsWhenPoolExhausted(t *testing.T) {
	pool := newFakePool(10)
	if err := pool.Hold(10); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, "deep", pool, newDep(t, http.StatusOK).URL)
	if rec := get(h, "/health"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHealthDeepFailsWhenDepDown(t *testing.T) {
	h := newTestServer(t, "deep", newFakePool(10), closedDep(t).URL)
	if rec := get(h, "/health"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
