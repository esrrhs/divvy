package tools

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// CommitAll stages every change under dir and creates one commit.
// It reports whether a commit was created and its short hash.
func CommitAll(dir, msg string) (committed bool, hash string, err error) {
	if !IsRepo(dir) {
		return false, "", fmt.Errorf("%s is not a git repository", dir)
	}

	add := exec.Command("git", "-C", dir, "add", "-A")
	var addErr bytes.Buffer
	add.Stderr = &addErr
	if err := add.Run(); err != nil {
		return false, "", fmt.Errorf("git add: %v: %s", err, strings.TrimSpace(addErr.String()))
	}

	var commitOut bytes.Buffer
	commit := exec.Command("git", "-C", dir, "commit", "-m", msg)
	commit.Stdout = &commitOut
	commit.Stderr = &commitOut
	if err := commit.Run(); err != nil {
		if strings.Contains(commitOut.String(), "nothing to commit") {
			return false, "", nil
		}
		return false, "", fmt.Errorf("git commit: %v: %s", err, strings.TrimSpace(commitOut.String()))
	}

	rev := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD")
	out, err := rev.Output()
	if err != nil {
		return true, "", nil // committed, but hash unknown
	}
	return true, strings.TrimSpace(string(out)), nil
}
