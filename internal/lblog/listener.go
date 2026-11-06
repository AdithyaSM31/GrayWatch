package lblog

import (
	"context"
	"net"
	"time"
)

const (
	maxDatagram = 2048
	// readBuffer holds several seconds of log lines at the benchmark's request
	// rate, so a brief stall in this process does not lose them.
	readBuffer = 1 << 20
)

// Listen receives HAProxy's syslog datagrams into w until ctx ends. It returns
// nil when ctx ends, and an error if it cannot bind or stops reading.
func Listen(ctx context.Context, addr string, w *Window) error {
	return ListenFrom(ctx, addr, w, nil)
}

// ListenFrom is Listen, keeping only datagrams from allow. Nil allows any source.
func ListenFrom(ctx context.Context, addr string, w *Window, allow *net.IPNet) error {
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if udp, ok := conn.(*net.UDPConn); ok {
		udp.SetReadBuffer(readBuffer)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if allow != nil {
			if udp, ok := from.(*net.UDPAddr); !ok || !allow.Contains(udp.IP) {
				continue
			}
		}
		if r, ok := ParseLine(string(buf[:n]), time.Now()); ok {
			w.Add(r)
		}
	}
}
