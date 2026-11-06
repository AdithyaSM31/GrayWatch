// Command graywatchd finds gray failures and drains the affected containers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/daemon"
	"github.com/AdithyaSM31/graywatch/internal/detect"
	"github.com/AdithyaSM31/graywatch/internal/ebpf"
	"github.com/AdithyaSM31/graywatch/internal/events"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/lblog"
	"github.com/AdithyaSM31/graywatch/internal/metrics"
	"github.com/AdithyaSM31/graywatch/internal/remediate"
)

func main() {
	addr := flag.String("haproxy", "127.0.0.1:9999", "HAProxy runtime API address")
	backend := flag.String("backend", "be_app", "HAProxy backend")
	logListen := flag.String("log-listen", ":5140", "UDP address for HAProxy's request log")
	interval := flag.Duration("interval", time.Second, "tick interval")
	metricsAddr := flag.String("metrics", ":9100", "Prometheus metrics address")
	eventsPath := flag.String("events", "events.jsonl", "event log file")
	dryRun := flag.Bool("dry-run", false, "detect and report, but change nothing in HAProxy")
	startupGrace := flag.Duration("startup-grace", 20*time.Second, "hold drains for this long after traffic starts")
	stateFile := flag.String("state-file", "/var/lib/graywatch/graywatchd.json", "where to remember this daemon's own drains")
	logFrom := flag.String("log-from", "", "accept the request log only from this CIDR (default: from anywhere)")
	useEBPF := flag.Bool("ebpf", false, "add the run queue latency signal, which traces every context switch")
	flag.Parse()

	var allow *net.IPNet
	if *logFrom != "" {
		_, cidr, err := net.ParseCIDR(*logFrom)
		if err != nil {
			log.Fatalf("-log-from: %v", err)
		}
		allow = cidr
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr, *backend, *logListen, allow, *metricsAddr, *eventsPath, *stateFile, *interval, *startupGrace, *dryRun, *useEBPF); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr, backend, logListen string, logFrom *net.IPNet, metricsAddr, eventsPath, stateFile string, interval, startupGrace time.Duration, dryRun, useEBPF bool) error {
	client := &haproxy.Client{Addr: addr, Timeout: 2 * time.Second}
	stats, err := client.ShowStat(ctx, backend)
	if err != nil {
		return fmt.Errorf("reading the fleet from HAProxy: %w", err)
	}
	if len(stats) == 0 {
		return fmt.Errorf("backend %s has no servers", backend)
	}

	file, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	policy := remediate.DefaultPolicy()
	policy.DryRun = dryRun
	saved, err := remediate.LoadState(stateFile)
	if err != nil {
		// Nothing is ever restored by mistake this way: unknown drains stay out.
		log.Printf("reading %s, so treating every drained server as someone else's: %v", stateFile, err)
	}
	planner := remediate.NewPlanner(policy, len(stats), daemon.OwnDrains(saved, stats), time.Now())
	planner.LoadHistory(saved.History)

	window := lblog.NewWindow(time.Minute)
	registry := prometheus.NewRegistry()
	collector := collect.New(collect.FSKernelSource{}, window, time.Now)
	if useEBPF {
		src, err := ebpf.Load()
		if err != nil {
			return fmt.Errorf("-ebpf: %w", err)
		}
		defer src.Close()
		collector.SetRunq(src, ebpf.CgroupID)
	}
	d := daemon.New(daemon.Deps{
		Discover: func(ctx context.Context) ([]fleet.Host, error) {
			hosts, err := fleet.Discover(ctx, fleet.CLIDocker{}, "/proc")
			if err == nil && len(hosts) > 0 {
				names := make([]string, len(hosts))
				for i, h := range hosts {
					names[i] = h.Name
				}
				window.KeepOnly(names)
			}
			return hosts, err
		},
		Collector: collector,
		Detector:  detect.New(detect.DefaultConfig()),
		Planner:   planner,
		Actuator:  &remediate.HAProxyActuator{C: client, Backend: backend},
		Events:    events.NewWriter(file),
		Metrics:   metrics.New(registry),
		Now:       time.Now,

		StartupGrace: startupGrace,
		Servers: func(ctx context.Context) ([]haproxy.ServerStat, error) {
			return client.ShowStat(ctx, backend)
		},
		SaveState: func(s remediate.State) error { return remediate.SaveState(stateFile, s) },
	})
	d.SyncState()

	errc := make(chan error, 2)
	go func() { errc <- lblog.ListenFrom(ctx, logListen, window, logFrom) }()
	server := &http.Server{Addr: metricsAddr, Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{})}
	go func() { errc <- server.ListenAndServe() }()
	defer server.Close()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		case <-ticker.C:
			d.Step(ctx)
		}
	}
}
