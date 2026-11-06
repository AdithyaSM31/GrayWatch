package chaos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// hookTimeout bounds an admin hook call. A service under a fault can be slow to
// answer, and a revert must not hang with it.
const hookTimeout = 10 * time.Second

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var hookClient = &http.Client{Timeout: hookTimeout}

// HTTPPost calls an admin hook. Any status other than 2xx is an error.
func HTTPPost(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := hookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
