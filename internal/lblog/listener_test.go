package lblog

import (
	"context"
	"net"
	"testing"
	"time"
)

// freeUDPAddr finds a port that is free right now.
func freeUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().String()
}

func startListener(t *testing.T, addr string, w *Window) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Listen(ctx, addr, w) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return done
}

// waitFor resends the datagram until it lands, because the listener may still
// be binding when the first one goes out.
func waitFor(t *testing.T, addr, line string, w *Window, server string, want int) {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.Write([]byte(line))
		time.Sleep(10 * time.Millisecond)
		if _, _, n := w.Stats(server, time.Now(), time.Minute); n >= want {
			return
		}
	}
	t.Fatalf("no datagram for %s reached the window", server)
}

func TestListenerReceivesUDP(t *testing.T) {
	addr := freeUDPAddr(t)
	w := NewWindow(time.Minute)
	startListener(t, addr, w)
	waitFor(t, addr, "<134>Nov  2 10:00:00 haproxy[8]: gw app-03 12 200\n", w, "app-03", 1)

	p90, errRate, _ := w.Stats("app-03", time.Now(), time.Minute)
	if p90 != 12 || errRate != 0 {
		t.Fatalf("got p90=%v errRate=%v, want 12, 0", p90, errRate)
	}
}

func TestListenerIgnoresOtherLines(t *testing.T) {
	addr := freeUDPAddr(t)
	w := NewWindow(time.Minute)
	startListener(t, addr, w)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("<133>Nov  2 10:00:00 haproxy[8]: Proxy fe_app started."))
	waitFor(t, addr, "gw app-03 12 200", w, "app-03", 1)

	if _, _, n := w.Stats("fe_app", time.Now(), time.Minute); n != 0 {
		t.Fatalf("n = %d, want the startup line ignored", n)
	}
}

func TestListenStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Listen(ctx, freeUDPAddr(t), NewWindow(time.Minute)) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Listen returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not stop")
	}
}

func TestListenReportsBindFailure(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Listen(context.Background(), conn.LocalAddr().String(), NewWindow(time.Minute)); err == nil {
		t.Fatal("want error for a port already in use")
	}
}
