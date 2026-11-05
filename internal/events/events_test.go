package events

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func TestEventsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	in := []Event{
		{TS: t0, Type: Suspect, Host: "app-07", Category: "disk", Reason: "io", Signals: []string{"io_psi"}, Score: 33.5},
		{TS: t0.Add(time.Second), Type: Drain, Host: "app-07", DryRun: true},
		{TS: t0.Add(2 * time.Second), Type: FleetAlert, Reason: "median latency"},
	}
	for _, e := range in {
		if err := w.Emit(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != 3 {
		t.Fatalf("got %d lines, want one per event:\n%s", got, buf.String())
	}
	out, err := ReadAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d events", len(out))
	}
	first := out[0]
	if !first.TS.Equal(t0) || first.Type != Suspect || first.Host != "app-07" || first.Category != "disk" ||
		first.Reason != "io" || len(first.Signals) != 1 || first.Signals[0] != "io_psi" || first.Score != 33.5 {
		t.Errorf("first = %+v", first)
	}
	if !out[1].DryRun || out[1].Type != Drain {
		t.Errorf("second = %+v", out[1])
	}
}

func TestEventTypeNames(t *testing.T) {
	for got, want := range map[string]string{
		Suspect: "suspect", Healthy: "healthy", Drain: "drain", Restore: "restore", FleetAlert: "fleet_alert",
		CapReached: "cap_reached", Flapping: "flapping", ActionError: "action_error",
	} {
		if got != want {
			t.Errorf("type %q, want %q", got, want)
		}
	}
}

func TestReadAllEmpty(t *testing.T) {
	out, err := ReadAll(strings.NewReader(""))
	if err != nil || len(out) != 0 {
		t.Fatalf("got %v, %v", out, err)
	}
}

// A daemon killed mid-write leaves a torn last line. The events before it are
// still good, and the benchmark needs them.
func TestReadAllToleratesTornLastLine(t *testing.T) {
	out, err := ReadAll(strings.NewReader(`{"type":"suspect","host":"app-01"}` + "\n" + `{"type":"dra`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Host != "app-01" {
		t.Fatalf("got %+v", out)
	}
}

func TestReadAllRejectsCorruptMiddleLine(t *testing.T) {
	if _, err := ReadAll(strings.NewReader("{bad\n" + `{"type":"suspect"}` + "\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestReadAllHandlesLongLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	long := strings.Repeat("x", 1<<20)
	if err := w.Emit(Event{Type: ActionError, Reason: long}); err != nil {
		t.Fatal(err)
	}
	out, err := ReadAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out[0].Reason) != len(long) {
		t.Fatal("long line not read back")
	}
}

func TestEmitIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Emit(Event{Type: Suspect, Host: "app-01"})
		}()
	}
	wg.Wait()
	out, err := ReadAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 50 {
		t.Fatalf("got %d events, want 50", len(out))
	}
}
