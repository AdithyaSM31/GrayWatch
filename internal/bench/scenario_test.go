package bench

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/chaos"
)

const scenarioDir = "../../bench/scenarios"

func TestLoadScenarios(t *testing.T) {
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) != 10 {
		t.Fatalf("got %d scenarios, want 10", len(scs))
	}
	byName := map[string]Scenario{}
	for _, sc := range scs {
		byName[sc.Name] = sc
	}
	if got := byName["pool-leak"].ExpectedCategory; got != "app" {
		t.Errorf("pool-leak category = %q, want app", got)
	}
	if got := byName["dep-slow"].Target; got != TargetDep {
		t.Errorf("dep-slow target = %q, want dep", got)
	}
	none := byName["none"]
	if none.Target != TargetNone || none.Fault != "" || none.FaultDur != 600*time.Second {
		t.Errorf("none = %+v", none)
	}
}

// Each fault's expected category must be the one the fault itself declares.
func TestScenariosMatchTheFaultRegistry(t *testing.T) {
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := chaos.Registry()
	seen := 0
	for _, sc := range scs {
		if sc.Target == TargetNone {
			continue
		}
		seen++
		f, ok := registry[sc.Fault]
		if !ok {
			t.Errorf("%s: unknown fault %q", sc.Name, sc.Fault)
			continue
		}
		if f.Category() != sc.ExpectedCategory {
			t.Errorf("%s: expected category %q, fault says %q", sc.Name, sc.ExpectedCategory, f.Category())
		}
		if sc.Baseline != 30*time.Second || sc.FaultDur != 90*time.Second || sc.Cooldown != 30*time.Second {
			t.Errorf("%s: timings = %v %v %v", sc.Name, sc.Baseline, sc.FaultDur, sc.Cooldown)
		}
	}
	if seen != len(registry) {
		t.Errorf("%d fault scenarios, registry has %d faults", seen, len(registry))
	}
}

func TestLoadScenariosAreSortedByName(t *testing.T) {
	scs, err := LoadScenarios(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(scs, func(a, b Scenario) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("not sorted: %+v", scs)
	}
}

func TestLoadScenariosRejectsBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":    "name: x\nfault: pool-leak\ntarget: random\nfaultdur: 1s\ncolour: red\n",
		"no name":          "fault: pool-leak\ntarget: random\nfaultdur: 1s\n",
		"bad target":       "name: x\nfault: pool-leak\ntarget: someone\nfaultdur: 1s\n",
		"fault, no target": "name: x\nfault: pool-leak\nfaultdur: 1s\n",
		"none with fault":  "name: x\nfault: pool-leak\ntarget: none\nfaultdur: 1s\n",
		"random, no fault": "name: x\ntarget: random\nfaultdur: 1s\n",
		"unknown fault":    "name: x\nfault: gremlins\ntarget: random\nfaultdur: 1s\n",
		"no duration":      "name: x\nfault: pool-leak\ntarget: random\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadScenarios(dir); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLoadScenariosRejectsEmptyDir(t *testing.T) {
	if _, err := LoadScenarios(t.TempDir()); err == nil {
		t.Fatal("want error")
	}
}

func TestSelectScenarios(t *testing.T) {
	scs := []Scenario{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, err := SelectScenarios(scs, "c, a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("got %+v", got)
	}
	if all, err := SelectScenarios(scs, ""); err != nil || len(all) != 3 {
		t.Fatalf("empty filter: %v, %v", all, err)
	}
	if _, err := SelectScenarios(scs, "a,typo"); err == nil {
		t.Fatal("a typo must not silently shrink the run")
	}
}
