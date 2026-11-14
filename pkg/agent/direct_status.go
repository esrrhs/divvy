package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/tools"
)

// directStatusTimeout bounds the git status probe. It is a safety net, not a
// step: a big repository's `git status` can take seconds, and the probe must
// not eat the turn. When it does time out, the model is told the status is
// unknown rather than left to assume the workspace is clean.
const directStatusTimeout = 8 * time.Second

// directStatusProbe runs the git status probe. It is a variable so a test can
// simulate a repository whose status cannot be read.
var directStatusProbe = func(ctx context.Context, root string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "--untracked-files=normal").Output()
}

// clipRunes cuts a string by runes, so a multi-byte character is never split
// in half — a broken rune reaches the model as a replacement character.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// directWorkspaceStatus summarizes uncommitted changes at the start of a
// direct turn. Cursor shows the change list next to the chat; the agent needs
// the same context so it does not mistake a user's pending edit (or work left
// by an earlier turn) for a clean workspace. The output is deliberately small:
// this is orientation, not a diff.
func directWorkspaceStatus(root string) string {
	if root == "" || !tools.IsRepo(root) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), directStatusTimeout)
	defer cancel()
	out, err := directStatusProbe(ctx, root)
	if err != nil {
		// This block is a safety net: it tells the model there may be edits it
		// must not overwrite. If it cannot be read, saying nothing reads as
		// "the workspace is clean" — the opposite of the truth, and the model
		// would then overwrite the user's work. Say that it is unknown.
		return "Uncommitted changes at the start of this turn: COULD NOT BE READ " +
			"(git status failed or timed out). Assume the workspace may contain pending edits " +
			"and inspect before overwriting anything."
	}
	lines := make([]string, 0, 16)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > 200 {
			line = clipRunes(line, 200)
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
