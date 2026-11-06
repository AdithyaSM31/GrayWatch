package netstat

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snmp")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTCPCounters(t *testing.T) {
	got, err := ReadTCPCounters("testdata/snmp")
	if err != nil {
		t.Fatal(err)
	}
	if want := (TCPCounters{OutSegs: 875, RetransSegs: 0}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Column positions differ between kernels, and "Udp:" has the same shape, so
// the reader must pair the two "Tcp:" lines by header name.
func TestReadTCPCountersPairsByName(t *testing.T) {
	path := writeTemp(t, "Ip: Forwarding OutSegs\nIp: 1 999\n"+
		"Tcp: RetransSegs InSegs OutSegs\nTcp: 7 100 200\n"+
		"Udp: OutSegs RetransSegs\nUdp: 888 777\n")
	got, err := ReadTCPCounters(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := (TCPCounters{OutSegs: 200, RetransSegs: 7}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadTCPCountersRejectsBadInput(t *testing.T) {
	for name, content := range map[string]string{
		"no Tcp lines":    "Ip: Forwarding\nIp: 1\n",
		"header only":     "Tcp: OutSegs RetransSegs\n",
		"missing column":  "Tcp: OutSegs\nTcp: 200\n",
		"length mismatch": "Tcp: OutSegs RetransSegs\nTcp: 200\n",
		"bad value":       "Tcp: OutSegs RetransSegs\nTcp: abc 7\n",
		"negative value":  "Tcp: OutSegs RetransSegs\nTcp: -1 7\n",
	} {
		if _, err := ReadTCPCounters(writeTemp(t, content)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ReadTCPCounters(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: want error")
	}
}
