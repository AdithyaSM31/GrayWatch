package lblog

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestListenFromDropsOtherSources(t *testing.T) {
	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		allow    *net.IPNet
		accepted bool
	}{
		"allowed": {loopback, true},
		"dropped": {other, false},
		"no list": {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			addr := freeUDPAddr(t)
			w := NewWindow(time.Minute)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- ListenFrom(ctx, addr, w, tc.allow) }()

			conn, err := net.Dial("udp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// UDP has no handshake, so keep sending until the listener must be up.
			for range 30 {
				conn.Write([]byte("gw app-01 10 200"))
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := w.held("app-01"); (got > 0) != tc.accepted {
				t.Fatalf("held %d, accepted should be %v", got, tc.accepted)
			}
		})
	}
}
