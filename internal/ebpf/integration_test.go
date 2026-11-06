//go:build integration && linux

package ebpf

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/chaos"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

// The plan's check: cpu-contention on app-03 raises its run queue latency above the
// fleet's. It needs root and a running fleet.
func TestContentionRaisesRunqLatencyAboveTheFleet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	hosts, err := fleet.Discover(ctx, fleet.CLIDocker{}, "/proc")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 20 {
		t.Fatalf("got %d hosts, want a running fleet of 20", len(hosts))
	}
	ids := map[string]uint64{}
	for _, h := range hosts {
		if ids[h.Name], err = CgroupID(h.CgroupDir); err != nil {
			t.Fatal(err)
		}
	}

	results := make(chan loadgen.Result)
	go func() {
		for range results {
		}
	}()
	go loadgen.Run(ctx, loadgen.Config{URL: "http://localhost:8000/work", RPS: 600, Timeout: 2 * time.Second}, results)

	read := func(window time.Duration) (target, median float64) {
		t.Helper()
		src.P99ByCgroup() // start the window here
		time.Sleep(window)
		p99s, err := src.P99ByCgroup()
		if err != nil {
			t.Fatal(err)
		}
		var peers []float64
		for name, id := range ids {
			if name == "app-03" {
				target = p99s[id]
			} else if p, ok := p99s[id]; ok {
				peers = append(peers, p)
			}
		}
		slices.Sort(peers)
		if len(peers) < 10 {
			t.Fatalf("only %d peers have data", len(peers))
		}
		return target, peers[len(peers)/2]
	}

	time.Sleep(15 * time.Second) // past the first slow requests
	before, medianBefore := read(5 * time.Second)
	t.Logf("before the fault: app-03 %v us, fleet median %v us", before, medianBefore)

	env := chaos.Env{Run: chaos.ExecRunner{}, Post: chaos.HTTPPost, StateDir: t.TempDir()}
	h, err := fleet.Lookup(ctx, fleet.CLIDocker{}, "/proc", "app-03")
	if err != nil {
		t.Fatal(err)
	}
	f := chaos.Registry()["cpu-contention"]
	if err := chaos.ApplyWithState(ctx, env, f, h); err != nil {
		t.Fatal(err)
	}
	defer chaos.RevertAll(context.Background(), env)
	time.Sleep(3 * time.Second)

	during, medianDuring := read(5 * time.Second)
	t.Logf("during the fault: app-03 %v us, fleet median %v us", during, medianDuring)
	if during <= medianDuring {
		t.Fatalf("app-03 %v us is not above the fleet median %v us", during, medianDuring)
	}
}
