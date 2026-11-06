//go:build unix

package bench

import (
	"os/exec"
	"syscall"
)

// detachFromTerminalSignals puts cmd in its own process group, so a Ctrl-C at the
// terminal reaches only the benchmark, which then stops cmd in the right order.
func detachFromTerminalSignals(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
