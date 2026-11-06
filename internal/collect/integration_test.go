//go:build integration

package collect

import (
	"os"
	"testing"
	"time"

	"github.com/AdithyaSM31/graywatch/internal/cgroup"
	"github.com/AdithyaSM31/graywatch/internal/fleet"
)

// TestCollectorOnThisKernel reads real files on any Linux host and needs no
// Docker. It uses PID 1's cgroup because the root cgroup has no pressure files.
func TestCollectorOnThisKernel(t *testing.T) {
	dir, err := cgroup.DirForPID("/proc", 1)
	if err != nil {
		t.Fatal(err)
	}
	c := New(FSKernelSource{}, &fakeClient{stats: map[string]clientStats{}}, time.Now)
	c.SetHosts([]fleet.Host{{Name: "self", PID: os.Getpid(), CgroupDir: dir}})

	if s := c.Tick()[0]; len(s.Values) != 0 {
		t.Fatalf("first tick values = %v, want none", s.Values)
	}
	for start := time.Now(); time.Since(start) < time.Second; {
	}
	s := c.Tick()[0]
	t.Logf("cgroup %s: %v", dir, s.Values)
	for _, sig := range []Signal{SigCPUPSI, SigIOPSI, SigMemPSI, SigThrottle} {
		v, ok := s.Values[sig]
		if !ok {
			t.Errorf("%s missing", sig)
			continue
		}
		if v < 0 || v > 1.5 {
			t.Errorf("%s = %v, want a fraction of the tick", sig, v)
		}
	}
}
