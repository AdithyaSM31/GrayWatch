// Command svc is the demo application container.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/svc"
)

const usage = `Usage: svc

svc is configured through the environment:
  PORT=8080                 traffic port (/work, /health)
  ADMIN_PORT=9090           fault hook port (/admin/...)
  DB_URL                    Postgres URL (required)
  DEP_URL=http://dep:8081/  dependency URL
  HEALTH_MODE=shallow       shallow or deep
  AUDIT_PATH=/data/audit.log
  POOL_SIZE=10
  WORK_CPU_MS=3
`

func main() {
	flag.Usage = func() { fmt.Fprint(flag.CommandLine.Output(), usage) }
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, v)
	}
	return n, nil
}

func run() error {
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		return errors.New("DB_URL is required")
	}
	healthMode := env("HEALTH_MODE", "shallow")
	if healthMode != "shallow" && healthMode != "deep" {
		return fmt.Errorf("HEALTH_MODE must be shallow or deep, got %q", healthMode)
	}
	poolSize, err := envInt("POOL_SIZE", 10)
	if err != nil {
		return err
	}
	workCPUMs, err := envInt("WORK_CPU_MS", 3)
	if err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := svc.NewPGPool(ctx, dbURL, int32(poolSize))
	if err != nil {
		return err
	}
	depClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	traffic := &http.Server{
		Addr: ":" + env("PORT", "8080"),
		Handler: svc.NewServer(svc.Config{
			Hostname:   hostname,
			DepURL:     env("DEP_URL", "http://dep:8081/"),
			HealthMode: healthMode,
			AuditPath:  env("AUDIT_PATH", "/data/audit.log"),
			WorkCPU:    time.Duration(workCPUMs) * time.Millisecond,
		}, pool, depClient),
	}
	admin := &http.Server{
		Addr:    ":" + env("ADMIN_PORT", "9090"),
		Handler: svc.NewAdmin(pool, &svc.Burner{}, &svc.MemHog{}),
	}

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{traffic, admin} {
		go func() { errc <- srv.ListenAndServe() }()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(traffic.Shutdown(shutdownCtx), admin.Shutdown(shutdownCtx))
}
