//go:build unix

package chaos

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunnerSucceeds(t *testing.T) {
	if err := (ExecRunner{}).Run(context.Background(), "sh", "-c", "exit 0"); err != nil {
		t.Fatal(err)
	}
}

// tc and nsenter explain failures on stderr, so the error has to carry it.
func TestExecRunnerReportsStderr(t *testing.T) {
	// The text is assembled by the command, so it cannot match the error's copy
	// of the arguments.
	err := ExecRunner{}.Run(context.Background(), "sh", "-c", "printf 'RTNETLINK %s' nope >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "RTNETLINK nope") {
		t.Fatalf("err = %v, want it to include stderr", err)
	}
}
