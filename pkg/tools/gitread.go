package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// git runs args against the workspace repository and returns trimmed output.
func (s *Sandbox) git(ctx context.Context, args ...string) (string, error) {
	if !IsRepo(s.Root) {
		return "", fmt.Errorf("not a git repository: %s", s.Root)
	}
	full := append([]string{"-C", s.Root}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s", text)
	}
	if text == "" {
		text = "(clean/empty)"
	}
	return text, nil
}

// GitStatus returns porcelain v1 status plus branch info.
func (s *Sandbox) GitStatus(ctx context.Context) (string, error) {
	return s.git(ctx, "status", "-sb")
}

// GitDiff returns a unified diff (staged or working tree), limited to path
// when given.
func (s *Sandbox) GitDiff(ctx context.Context, staged bool, path string) (string, error) {
	args := []string{"--no-pager", "diff"}
	if staged {
		args = append(args, "--cached")
	}
	if strings.TrimSpace(path) != "" {
		args = append(args, "--", path)
	}
	return s.git(ctx, args...)
}

// GitLog returns recent oneline commits, limited to path when given.
func (s *Sandbox) GitLog(ctx context.Context, limit int, path string) (string, error) {
	if limit <= 0 {
		limit = 10
	}
	args := []string{"log", "-n", fmt.Sprint(limit), "--oneline"}
	if strings.TrimSpace(path) != "" {
		args = append(args, "--", path)
	}
	return s.git(ctx, args...)
}
