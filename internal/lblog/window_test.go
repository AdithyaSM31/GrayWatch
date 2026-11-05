package lblog

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func TestParseLine(t *testing.T) {
	got, ok := ParseLine("<134>Nov  2 10:00:00 haproxy[8]: gw app-03 12 200", t0)
	if !ok {
		t.Fatal("line rejected")
	}
	want := Req{Server: "app-03", TrMs: 12, Status: 200, At: t0}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseLineWithTrailingNewline(t *testing.T) {
	got, ok := ParseLine("<134>Nov  2 10:00:00 haproxy[8]: gw app-03 12 200\n", t0)
	if !ok || got.Status != 200 {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

// A request with no server response logs -1, which is exactly the slow or dead
// case the detector needs to see.
func TestParseLineKeepsNoResponseRequests(t *testing.T) {
	got, ok := ParseLine("<134>Nov  2 10:00:00 haproxy[8]: gw app-03 -1 504", t0)
	if !ok || got.TrMs != -1 || got.Status != 504 {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if got, ok := ParseLine("gw app-03 -1 -1", t0); !ok || got.Status != -1 {
		t.Fatalf("client abort: got %+v, %v", got, ok)
	}
}

func TestParseLineRejects(t *testing.T) {
	for _, line := range []string{
		"",
		"gw app-03 x 200",
		"gw app-03 12 x",
		"gw <NOSRV> -1 503",
		"gw app-03 12",
		"gw app-03 12 200 extra",
		"gw extra app-03 12 200",
		"<133>Nov  2 10:00:00 haproxy[8]: Proxy fe_app started.",
		"app-03 12 200 and more",
	} {
		if got, ok := ParseLine(line, t0); ok {
			t.Errorf("%q accepted as %+v", line, got)
		}
	}
}

func TestWindowStats(t *testing.T) {
	w := NewWindow(time.Minute)
	for i := 0; i < 9; i++ {
		w.Add(Req{Server: "app-01", TrMs: 10, Status: 200, At: t0})
	}
	w.Add(Req{Server: "app-01", TrMs: -1, Status: 504, At: t0})
	p90, errRate, n := w.Stats("app-01", t0.Add(time.Second), 5*time.Second)
	if p90 != 10 || errRate != 0.1 || n != 10 {
		t.Fatalf("got p90=%v errRate=%v n=%d, want 10, 0.1, 10", p90, errRate, n)
	}
}

func TestWindowTreatsNoResponseAs2000ms(t *testing.T) {
	w := NewWindow(time.Minute)
	w.Add(Req{Server: "app-01", TrMs: -1, Status: 200, At: t0})
	p90, errRate, n := w.Stats("app-01", t0, time.Second)
	if p90 != 2000 || errRate != 1 || n != 1 {
		t.Fatalf("got p90=%v errRate=%v n=%d, want 2000, 1, 1", p90, errRate, n)
	}
}

func TestWindowCountsErrors(t *testing.T) {
	for status, isErr := range map[int]bool{200: false, 404: false, 499: false, 500: true, 503: true, 0: true, -1: true} {
		w := NewWindow(time.Minute)
		w.Add(Req{Server: "app-01", TrMs: 5, Status: status, At: t0})
		_, errRate, _ := w.Stats("app-01", t0, time.Second)
		if (errRate == 1) != isErr {
			t.Errorf("status %d: errRate = %v, want error=%v", status, errRate, isErr)
		}
	}
}

func TestWindowP90IsNearestRank(t *testing.T) {
	w := NewWindow(time.Minute)
	for ms := 1; ms <= 20; ms++ {
		w.Add(Req{Server: "app-01", TrMs: ms, Status: 200, At: t0})
	}
	if p90, _, _ := w.Stats("app-01", t0, time.Second); p90 != 18 {
		t.Fatalf("p90 = %v, want 18", p90)
	}
}

func TestWindowSeparatesServers(t *testing.T) {
	w := NewWindow(time.Minute)
	w.Add(Req{Server: "app-01", TrMs: 10, Status: 200, At: t0})
	w.Add(Req{Server: "app-02", TrMs: 99, Status: 500, At: t0})
	p90, errRate, n := w.Stats("app-01", t0, time.Second)
	if p90 != 10 || errRate != 0 || n != 1 {
		t.Fatalf("got p90=%v errRate=%v n=%d", p90, errRate, n)
	}
}

func TestWindowUnknownServerIsEmpty(t *testing.T) {
	p90, errRate, n := NewWindow(time.Minute).Stats("app-01", t0, time.Second)
	if p90 != 0 || errRate != 0 || n != 0 {
		t.Fatalf("got p90=%v errRate=%v n=%d, want zeros", p90, errRate, n)
	}
}

func TestWindowSpanAndPrune(t *testing.T) {
	w := NewWindow(time.Minute)
	w.Add(Req{Server: "app-01", TrMs: 500, Status: 200, At: t0})
	w.Add(Req{Server: "app-01", TrMs: 10, Status: 200, At: t0.Add(58 * time.Second)})

	// Outside the span: excluded from Stats but still held.
	p90, _, n := w.Stats("app-01", t0.Add(59*time.Second), 5*time.Second)
	if p90 != 10 || n != 1 {
		t.Fatalf("got p90=%v n=%d, want the newer entry only", p90, n)
	}
	if got := w.held("app-01"); got != 2 {
		t.Fatalf("held = %d, want 2 inside retain", got)
	}

	// Past retain: gone from memory.
	w.Add(Req{Server: "app-01", TrMs: 10, Status: 200, At: t0.Add(90 * time.Second)})
	if got := w.held("app-01"); got != 2 {
		t.Fatalf("held = %d, want 2 after the oldest aged out", got)
	}
}

// A drained host stops getting traffic, so no Add would ever prune its entries
// and its old requests would still count once it is restored.
func TestWindowStatsPrunesIdleServer(t *testing.T) {
	w := NewWindow(time.Minute)
	w.Add(Req{Server: "app-01", TrMs: 10, Status: 200, At: t0})
	if _, _, n := w.Stats("app-01", t0.Add(2*time.Minute), 3*time.Minute); n != 0 {
		t.Fatalf("n = %d, want entries past retain ignored even for a wide span", n)
	}
	if got := w.held("app-01"); got != 0 {
		t.Fatalf("held = %d, want 0", got)
	}
}
