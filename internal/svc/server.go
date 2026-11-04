// Package svc is the demo service: every /work request touches disk, the
// database, the network and the CPU, so each fault type has something to break.
package svc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	auditRecordSize = 4096
	healthTimeout   = time.Second
)

var auditRecord = append(bytes.Repeat([]byte{'a'}, auditRecordSize-1), '\n')

type Config struct {
	Hostname, DepURL, HealthMode, AuditPath string
	WorkCPU                                 time.Duration
}

type server struct {
	cfg  Config
	pool Pool
	dep  *http.Client

	auditOnce sync.Once
	audit     *os.File
	auditErr  error
}

func NewServer(cfg Config, pool Pool, dep *http.Client) http.Handler {
	s := &server{cfg: cfg, pool: pool, dep: dep}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", s.handleWork)
	mux.HandleFunc("GET /health", s.handleHealth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", cfg.Hostname)
		mux.ServeHTTP(w, r)
	})
}

func (s *server) handleWork(w http.ResponseWriter, r *http.Request) {
	if err := s.work(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) work(ctx context.Context) error {
	if err := s.writeAudit(); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if err := s.pool.Query(ctx); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	if err := s.callDep(ctx); err != nil {
		return fmt.Errorf("dep: %w", err)
	}
	for start := time.Now(); time.Since(start) < s.cfg.WorkCPU; {
	}
	return nil
}

// writeAudit fsyncs so that the disk faults reach the request path; without
// the sync the write would only land in the page cache.
func (s *server) writeAudit() error {
	s.auditOnce.Do(func() {
		s.audit, s.auditErr = os.OpenFile(s.cfg.AuditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	})
	if s.auditErr != nil {
		return s.auditErr
	}
	if _, err := s.audit.Write(auditRecord); err != nil {
		return err
	}
	return s.audit.Sync()
}

func (s *server) callDep(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.DepURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.dep.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.cfg.HealthMode != "deep" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := s.callDep(ctx); err != nil {
		http.Error(w, "dep: "+err.Error(), http.StatusServiceUnavailable)
	}
}
