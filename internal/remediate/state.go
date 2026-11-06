package remediate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// State is what the planner must remember across a restart. Without it, a daemon
// can't tell its own drains from servers a person took out, and a host that keeps
// flapping starts its count again.
type State struct {
	Drained []string               `json:"drained"`
	History map[string][]time.Time `json:"history"`
}

// State returns what to save. History older than the flap window can't matter.
func (pl *Planner) State(now time.Time) State {
	s := State{
		Drained: slices.Sorted(maps.Keys(pl.drained)),
		History: map[string][]time.Time{},
	}
	for host, times := range pl.history {
		for _, at := range times {
			if now.Sub(at) < pl.policy.FlapWindow {
				s.History[host] = append(s.History[host], at)
			}
		}
	}
	return s
}

// LoadHistory restores the drain history from an earlier run.
func (pl *Planner) LoadHistory(history map[string][]time.Time) {
	for host, times := range history {
		pl.history[host] = slices.Clone(times)
	}
}

// SaveState renames into place, so a crash can't leave a partial file.
func SaveState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState reads what SaveState wrote. A missing file means a first run.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}
