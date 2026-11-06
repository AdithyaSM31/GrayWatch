package collect

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
	"github.com/AdithyaSM31/graywatch/internal/netstat"
)

type KernelSnap struct {
	CPU, IO, Mem cgroup.PSI
	CPUStat      cgroup.CPUStat
	TCP          netstat.TCPCounters
}

type KernelSource interface {
	Read(h fleet.Host) (KernelSnap, error)
}

// ClientSource is satisfied by *lblog.Window.
type ClientSource interface {
	Stats(server string, now time.Time, span time.Duration) (p90ms, errRate float64, n int)
}

// FSKernelSource reads the real kernel files. ProcRoot defaults to /proc.
type FSKernelSource struct {
	ProcRoot string
}

func (s FSKernelSource) Read(h fleet.Host) (KernelSnap, error) {
	var snap KernelSnap
	var err error
	if snap.CPU, err = cgroup.ReadPSI(filepath.Join(h.CgroupDir, "cpu.pressure")); err != nil {
		return KernelSnap{}, err
	}
	if snap.IO, err = cgroup.ReadPSI(filepath.Join(h.CgroupDir, "io.pressure")); err != nil {
		return KernelSnap{}, err
	}
	if snap.Mem, err = cgroup.ReadPSI(filepath.Join(h.CgroupDir, "memory.pressure")); err != nil {
		return KernelSnap{}, err
	}
	if snap.CPUStat, err = cgroup.ReadCPUStat(filepath.Join(h.CgroupDir, "cpu.stat")); err != nil {
		return KernelSnap{}, err
	}
	proc := s.ProcRoot
	if proc == "" {
		proc = "/proc"
	}
	if snap.TCP, err = netstat.ReadTCPCounters(filepath.Join(proc, strconv.Itoa(h.PID), "net", "snmp")); err != nil {
		return KernelSnap{}, err
	}
	return snap, nil
}
