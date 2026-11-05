// Package bench injects each fault on a real fleet and scores what the load
// balancer's health checks and Graywatch each did about it.
package bench

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AdithyaSM31/graywatch/internal/chaos"
)

const (
	TargetRandom = "random" // one app host, picked with the run's seed
	TargetDep    = "dep"    // the shared dependency, which every host calls
	TargetNone   = "none"   // no fault, for the false-positive baseline
)

type Scenario struct {
	Name             string        `yaml:"name"`
	Fault            string        `yaml:"fault"`
	ExpectedCategory string        `yaml:"expected_category"`
	Target           string        `yaml:"target"`
	Baseline         time.Duration `yaml:"baseline"`
	FaultDur         time.Duration `yaml:"faultdur"`
	Cooldown         time.Duration `yaml:"cooldown"`
}

func (sc Scenario) validate() error {
	if sc.Name == "" {
		return errors.New("no name")
	}
	if sc.FaultDur <= 0 {
		return errors.New("faultdur must be positive")
	}
	switch sc.Target {
	case TargetNone:
		if sc.Fault != "" {
			return errors.New("target none cannot have a fault")
		}
		return nil
	case TargetRandom, TargetDep:
		if _, ok := chaos.Registry()[sc.Fault]; !ok {
			return fmt.Errorf("unknown fault %q", sc.Fault)
		}
		return nil
	default:
		return fmt.Errorf("target %q is not one of random, dep, none", sc.Target)
	}
}

// LoadScenarios reads every .yaml file in dir, sorted by scenario name.
func LoadScenarios(dir string) ([]Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no scenarios in %s", dir)
	}
	scs := make([]Scenario, 0, len(paths))
	for _, path := range paths {
		sc, err := loadScenario(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		scs = append(scs, sc)
	}
	slices.SortFunc(scs, func(a, b Scenario) int { return strings.Compare(a.Name, b.Name) })
	return scs, nil
}

func loadScenario(path string) (Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var sc Scenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&sc); err != nil {
		return Scenario{}, err
	}
	return sc, sc.validate()
}

// SelectScenarios keeps the named scenarios from a comma-separated list, in their
// original order. An empty list keeps them all. An unknown name is an error, so a
// typo cannot silently shrink a run that takes hours.
func SelectScenarios(scs []Scenario, only string) ([]Scenario, error) {
	if strings.TrimSpace(only) == "" {
		return scs, nil
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(only, ",") {
		wanted[strings.TrimSpace(name)] = true
	}
	var out []Scenario
	for _, sc := range scs {
		if wanted[sc.Name] {
			out = append(out, sc)
			delete(wanted, sc.Name)
		}
	}
	if len(wanted) > 0 {
		var unknown []string
		for name := range wanted {
			unknown = append(unknown, name)
		}
		slices.Sort(unknown)
		return nil, fmt.Errorf("unknown scenarios: %s", strings.Join(unknown, ", "))
	}
	return out, nil
}
