package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/tools"
)

// directWorkspaceStatus summarizes uncommitted changes at the start of a
// direct turn. Cursor shows the change list next to the chat; the agent needs
// the same context so it does not mistake a user's pending edit (or work left
// by an earlier turn) for a clean workspace. The output is deliberately small:
// this is orientation, not a diff.
func directWorkspaceStatus(root string) string {
	if root == "" || !tools.IsRepo(root) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "--untracked-files=normal").Output()
	if err != nil {
		return ""
	}
	lines := make([]string, 0, 16)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		lines = append(lines, line)
		if len(lines) >= 40 {
			lines = append(lines, "…(more changes omitted)")
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Uncommitted changes at the start of this turn (git status; may include work from earlier turns or the user — inspect before overwriting):\n" +
		strings.Join(lines, "\n")
}
