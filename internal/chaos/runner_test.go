package chaos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPPostSendsPost(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
	}))
	defer srv.Close()
	if err := HTTPPost(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %s, want POST", method)
	}
}

// The admin hooks answer 400 or 500 when they refuse, and a fault that silently
// did nothing would corrupt the benchmark.
func TestHTTPPostRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "hold 10: timed out", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := HTTPPost(context.Background(), srv.URL); err == nil {
		t.Fatal("want error")
	}
}

func TestHTTPPostHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := HTTPPost(ctx, srv.URL); err == nil {
		t.Fatal("want error")
	}
}
