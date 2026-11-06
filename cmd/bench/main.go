// Command bench injects each fault on a real fleet, scores what the health checks
// and Graywatch each did, and writes the results to a CSV file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/bench"
	"github.com/AdithyaSM31/graywatch/internal/chaos"
	"github.com/AdithyaSM31/graywatch/internal/haproxy"
	"github.com/AdithyaSM31/graywatch/internal/loadgen"
)

const usage = `Usage:
  bench run -scenarios bench/scenarios -trials 3 -configs shallow:detect,shallow:remediate,deep:detect -seed 42 -out results/<date> [-only pool-leak,net-loss]
  bench report [-scenarios bench/scenarios] [-out docs/RESULTS.md] results/<date>
`

const fleetSize = 20

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		err = run(ctx, os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// report turns a results directory's trials.csv into a Markdown file. A notes.md
// beside it is added at the end.
func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	scenarioDir := fs.String("scenarios", "bench/scenarios", "directory of scenario files")
	out := fs.String("out", filepath.Join("docs", "RESULTS.md"), "file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	dir := fs.Arg(0)

	f, err := os.Open(filepath.Join(dir, "trials.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	results, err := bench.ReadTrialsCSV(f)
	if err != nil {
		return err
	}

	scenarios, err := bench.LoadScenarios(*scenarioDir)
	if err != nil {
		return err
	}
	var window time.Duration
	for _, sc := range scenarios {
		if sc.Target == bench.TargetNone {
			window = sc.Baseline + sc.FaultDur + sc.Cooldown
		}
	}

	notes, err := os.ReadFile(filepath.Join(dir, "notes.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text, err := bench.Report(results, fleetSize, window, string(notes))
	if err != nil {
		return err
	}
	return os.WriteFile(*out, []byte(text), 0o644)
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	scenarioDir := fs.String("scenarios", "bench/scenarios", "directory of scenario files")
	trials := fs.Int("trials", 3, "trials of each fault scenario")
	configs := fs.String("configs", "shallow:detect,shallow:remediate,deep:detect", "health:mode pairs to run")
	seed := fs.Uint64("seed", 42, "seed for picking target hosts")
	out := fs.String("out", filepath.Join("results", time.Now().Format("2006-01-02")), "output directory")
	only := fs.String("only", "", "run only these scenarios")
	deployDir := fs.String("deploy", "deploy", "where the fleet's files and data go")
	daemonPath := fs.String("graywatchd", "graywatchd", "path to the graywatchd binary")
	addr := fs.String("haproxy", "127.0.0.1:9999", "HAProxy runtime API address")
	url := fs.String("url", "http://localhost:8000/work", "load target")
	rps := fs.Int("rps", 600, "requests per second")
	warmup := fs.Duration("warmup", time.Minute, "traffic before the first trial of each config")
	ioDevice := fs.String("io-device", "", "MAJ:MIN of the disk for io-throttle (default: the disk holding the fleet's data)")
	stateDir := fs.String("state-dir", "/var/lib/graywatch/chaos", "where applied faults are recorded")
	force := fs.Bool("force", false, "replace results already in the output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *trials < 1 {
		return errors.New("-trials must be at least 1")
	}
	if os.Geteuid() != 0 {
		return errors.New("bench must run as root: faults write to cgroups and enter network namespaces")
	}

	all, err := bench.LoadScenarios(*scenarioDir)
	if err != nil {
		return err
	}
	scenarios, err := bench.SelectScenarios(all, *only)
	if err != nil {
		return err
	}
	cfgs, err := bench.ParseConfigs(*configs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	csvPath := filepath.Join(*out, "trials.csv")
	if err := bench.CheckOutput(csvPath, *force); err != nil {
		return err
	}

	world := &bench.RealWorld{
		DeployDir:  *deployDir,
		DaemonPath: *daemonPath,
		FleetSize:  fleetSize,
		Backend:    "be_app",
		HAProxy:    &haproxy.Client{Addr: *addr, Timeout: 2 * time.Second},
		Chaos:      chaos.Env{Run: chaos.ExecRunner{}, Post: chaos.HTTPPost, IODevice: *ioDevice, StateDir: *stateDir},
		LoadConfig: loadgen.Config{URL: *url, RPS: *rps, Timeout: 2 * time.Second},
	}
	o := &bench.Orchestrator{
		World: world,
		Now:   time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
		Save:      func(rs []bench.TrialResult) error { return bench.SaveTrialsCSV(csvPath, rs) },
		Logf:      log.Printf,
		OutDir:    *out,
		Seed:      *seed,
		Warmup:    *warmup,
		PollEvery: time.Second,
	}

	log.Printf("%d scenarios, %d configs, about %v. Results go to %s", len(scenarios), len(cfgs),
		bench.EstimateDuration(scenarios, len(cfgs), *trials, *warmup).Round(time.Minute), csvPath)
	results, err := o.Run(ctx, scenarios, cfgs, *trials)
	log.Printf("%d trials finished, saved to %s", len(results), csvPath)
	return err
}
