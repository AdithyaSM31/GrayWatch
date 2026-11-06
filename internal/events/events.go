// Package events is the record of what the daemon saw and did, one JSON object
// per line. The benchmark scores a run from this file.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	Suspect     = "suspect"
	Healthy     = "healthy"
	Drain       = "drain"
	Restore     = "restore"
	FleetAlert  = "fleet_alert"
	CapReached  = "cap_reached"
	Flapping    = "flapping"
	ActionError = "action_error"
	DrainHeld   = "drain_held"
)

type Event struct {
	TS       time.Time `json:"ts"`
	Type     string    `json:"type"`
	Host     string    `json:"host,omitempty"`
	Category string    `json:"category,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Signals  []string  `json:"signals,omitempty"`
	Score    float64   `json:"score,omitempty"`
	DryRun   bool      `json:"dry_run,omitempty"`
}

type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

func (w *Writer) Emit(e Event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(append(line, '\n'))
	return err
}

// ReadAll reads every event. A malformed last line with no newline is skipped,
// since that is what a daemon killed mid-write leaves behind.
func ReadAll(r io.Reader) ([]Event, error) {
	br := bufio.NewReader(r)
	var out []Event
	for n := 1; ; n++ {
		line, readErr := br.ReadBytes('\n')
		atEnd := errors.Is(readErr, io.EOF)
		if readErr != nil && !atEnd {
			return nil, readErr
		}
		if len(line) > 0 {
			var e Event
			if err := json.Unmarshal(line, &e); err == nil {
				out = append(out, e)
			} else if !atEnd {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
		}
		if atEnd {
			return out, nil
		}
	}
}
