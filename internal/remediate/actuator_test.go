package remediate

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/haproxy"
)

// fakeRuntime is a scripted HAProxy runtime API. show stat replies with the next
// value of scur for app-03, repeating the last one. set commands get setReply.
type fakeRuntime struct {
	mu       sync.Mutex
	commands []string
	scurs    []int
	setReply func(cmd string) string
	addr     string
}

func newFakeRuntime(t *testing.T, scurs ...int) *fakeRuntime {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeRuntime{scurs: scurs, addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			conn.Write([]byte(f.reply(strings.TrimSpace(line))))
			conn.Close()
		}
	}()
	return f
}

func (f *fakeRuntime) reply(cmd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	if cmd == "show stat" {
		scur := 0
		if len(f.scurs) > 0 {
			scur = f.scurs[0]
			if len(f.scurs) > 1 {
				f.scurs = f.scurs[1:]
			}
		}
		return fmt.Sprintf("# pxname,svname,scur,status,\nbe_app,app-03,%d,DRAIN,\n", scur)
	}
	if f.setReply != nil {
		return f.setReply(cmd)
	}
	return "\n"
}

func (f *fakeRuntime) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func newActuator(f *fakeRuntime) *HAProxyActuator {
	return &HAProxyActuator{
		C:            &haproxy.Client{Addr: f.addr, Timeout: time.Second},
		Backend:      "be_app",
		pollInterval: 5 * time.Millisecond,
		drainWait:    200 * time.Millisecond,
	}
}

const (
	setDrain = "set server be_app/app-03 state drain"
	setMaint = "set server be_app/app-03 state maint"
	setReady = "set server be_app/app-03 state ready"
)

func TestHAProxyDrainThenMaint(t *testing.T) {
	f := newFakeRuntime(t, 2, 1, 0)
	if err := newActuator(f).Drain(context.Background(), "app-03"); err != nil {
		t.Fatal(err)
	}
	want := []string{setDrain, "show stat", "show stat", "show stat", setMaint}
	got := f.got()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

// In-flight requests get a limit, so that one stuck request cannot keep a sick
// host half-drained and stall the daemon's loop.
func TestDrainGoesToMaintAfterTimeout(t *testing.T) {
	f := newFakeRuntime(t, 5)
	start := time.Now()
	if err := newActuator(f).Drain(context.Background(), "app-03"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("took %v, want about the 200ms limit", elapsed)
	}
	got := f.got()
	if got[len(got)-1] != setMaint {
		t.Fatalf("commands = %q, want to end in maint", got)
	}
}

func TestDrainDefaultsAreFiveSeconds(t *testing.T) {
	a := HAProxyActuator{}
	if got := a.waitLimit(); got != 5*time.Second {
		t.Fatalf("limit = %v, want 5s", got)
	}
}

func TestDrainFailsIfDrainCommandFails(t *testing.T) {
	f := newFakeRuntime(t, 0)
	f.setReply = func(string) string { return "No such server.\n" }
	if err := newActuator(f).Drain(context.Background(), "app-03"); err == nil {
		t.Fatal("want error")
	}
	if got := f.got(); len(got) != 1 {
		t.Fatalf("commands = %q, want it to stop after the failure", got)
	}
}

// The planner will not count a failed drain, so HAProxy must not be left
// draining a host the cap does not know about.
func TestFailedMaintPutsServerBack(t *testing.T) {
	f := newFakeRuntime(t, 0)
	f.setReply = func(cmd string) string {
		if cmd == setMaint {
			return "failed\n"
		}
		return "\n"
	}
	if err := newActuator(f).Drain(context.Background(), "app-03"); err == nil {
		t.Fatal("want error")
	}
	got := f.got()
	if got[len(got)-1] != setReady {
		t.Fatalf("commands = %q, want to end in ready", got)
	}
}

func TestRestoreSetsReady(t *testing.T) {
	f := newFakeRuntime(t)
	if err := newActuator(f).Restore(context.Background(), "app-03"); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); len(got) != 1 || got[0] != setReady {
		t.Fatalf("commands = %q", got)
	}
}

func TestRestoreReportsFailure(t *testing.T) {
	f := newFakeRuntime(t)
	f.setReply = func(string) string { return "nope\n" }
	if err := newActuator(f).Restore(context.Background(), "app-03"); err == nil {
		t.Fatal("want error")
	}
}

func TestDrainStopsWhenContextEnds(t *testing.T) {
	f := newFakeRuntime(t, 5)
	a := newActuator(f)
	a.drainWait = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	a.Drain(ctx, "app-03")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v after the context ended", elapsed)
	}
}
