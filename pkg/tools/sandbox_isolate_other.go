//go:build !linux && !darwin

package tools

import (
	"context"
	"fmt"
	"os/exec"
)

func isolateShellCommand(ctx context.Context, root, command string, env []string) (*exec.Cmd, error) {
	return nil, fmt.Errorf("%w: run_bash requires filesystem isolation (bubblewrap on Linux, sandbox-exec on macOS) and is not available on this platform; the command was not executed", ErrExecutionUnavailable)
}

// ProbeShellIsolation always reports run_bash unavailable off Linux and macOS.
func ProbeShellIsolation(root string) (bool, string) {
	return false, "run_bash is unavailable on this platform: filesystem isolation is only implemented for Linux (bubblewrap) and macOS (sandbox-exec)"
}
