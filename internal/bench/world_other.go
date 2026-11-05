//go:build !unix

package bench

import "os/exec"

func detachFromTerminalSignals(*exec.Cmd) {}
