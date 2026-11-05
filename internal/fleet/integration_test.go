//go:build integration

package fleet

import (
	"context"
	"fmt"
	"testing"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/netstat"
)

// TestDiscoverRealFleet needs the fleet running on a Linux host, as root.
func TestDiscoverRealFleet(t *testing.T) {
	hosts, err := Discover(context.Background(), CLIDocker{}, "/proc")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 20 {
		t.Fatalf("got %d hosts, want 20", len(hosts))
	}
	for _, h := range hosts {
		if _, err := cgroup.ReadPSI(h.CgroupDir + "/cpu.pressure"); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
		if _, err := netstat.ReadTCPCounters(fmt.Sprintf("/proc/%d/net/snmp", h.PID)); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
	}
}

// TestReadersOnThisKernel needs no Docker: it runs the readers against the
// live kernel's own files, so it also works in WSL.
func TestReadersOnThisKernel(t *testing.T) {
	if psi, err := cgroup.ReadPSI("/proc/pressure/cpu"); err != nil {
		t.Errorf("ReadPSI: %v", err)
	} else {
		t.Logf("cpu pressure: %+v", psi)
	}
	if s, err := cgroup.ReadCPUStat("/sys/fs/cgroup/cpu.stat"); err != nil {
		t.Errorf("ReadCPUStat: %v", err)
	} else {
		t.Logf("cpu.stat: %+v", s)
	}
	if c, err := netstat.ReadTCPCounters("/proc/self/net/snmp"); err != nil {
		t.Errorf("ReadTCPCounters: %v", err)
	} else {
		t.Logf("tcp: %+v", c)
	}
	if dir, err := cgroup.DirForPID("/proc", 1); err != nil {
		t.Errorf("DirForPID: %v", err)
	} else {
		t.Logf("pid 1 cgroup: %s", dir)
	}
	if dev, err := cgroup.WholeDiskDevice("/sys", "/"); err != nil {
		t.Errorf("WholeDiskDevice: %v", err)
	} else {
		t.Logf("disk holding /: %s", dev)
	}
}
