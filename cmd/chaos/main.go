// Command chaos applies and reverts faults on running containers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/chaos"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

const usage = `Usage:
  chaos apply  -fault <name> -host <name> [-io-device MAJ:MIN] [-data-dir deploy/data] [-state-dir dir]
  chaos revert -fault <name> -host <name> [-state-dir dir]
  chaos revert-all [-state-dir dir]
  chaos list
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1], os.Args[2:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	faultName := fs.String("fault", "", "fault name")
	hostName := fs.String("host", "", "container name")
	ioDevice := fs.String("io-device", "", "MAJ:MIN of the disk for io-throttle (default: the disk holding -data-dir)")
	dataDir := fs.String("data-dir", "deploy/data", "directory whose disk io-throttle limits")
	stateDir := fs.String("state-dir", "/var/lib/graywatch/chaos", "where applied faults are recorded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := chaos.Env{Run: chaos.ExecRunner{}, Post: chaos.HTTPPost, IODevice: *ioDevice, StateDir: *stateDir, SameContainer: chaos.SameContainer("/proc")}

	switch command {
	case "apply":
		f, err := lookupFault(*faultName)
		if err != nil {
			return err
		}
		if *hostName == "" {
			return errors.New("-host is required")
		}
		if f.Name() == "io-throttle" && env.IODevice == "" {
			if env.IODevice, err = cgroup.WholeDiskDevice("/sys", *dataDir); err != nil {
				return err
			}
		}
		h, err := fleet.Lookup(ctx, fleet.CLIDocker{}, "/proc", *hostName)
		if err != nil {
			return err
		}
		return chaos.ApplyWithState(ctx, env, f, h)
	case "revert":
		if _, err := lookupFault(*faultName); err != nil {
			return err
		}
		if *hostName == "" {
			return errors.New("-host is required")
		}
		return chaos.RevertOne(ctx, env, *hostName, *faultName)
	case "revert-all":
		return chaos.RevertAll(ctx, env)
	case "list":
		registry := chaos.Registry()
		names := make([]string, 0, len(registry))
		for name := range registry {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fmt.Printf("%-16s %s\n", name, registry[name].Category())
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

func lookupFault(name string) (chaos.Fault, error) {
	registry := chaos.Registry()
	f, ok := registry[name]
	if !ok {
		names := make([]string, 0, len(registry))
		for n := range registry {
			names = append(names, n)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("unknown fault %q, want one of: %s", name, strings.Join(names, ", "))
	}
	return f, nil
}
