package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestDirectWorkspaceStatusListsChanges covers the "changes" context Cursor
// shows beside the chat: a direct turn must know the workspace is dirty, and
// must not be confused by ignored local files.
func TestDirectWorkspaceStatusListsChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n")
	runGitIn(t, root, "add", "a.go")
	runGitIn(t, root, "commit", "-q", "-m", "init")

	write("a.go", "package a\n\n// modified\n")
	write("b.txt", "untracked\n")
	write(".gitignore", "ignored.log\n")
	write("ignored.log", "local only\n")

	got := directWorkspaceStatus(root)
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.txt") {
		t.Fatalf("status should list modified and untracked files:\n%s", got)
	}
	if strings.Contains(got, "ignored.log") {
		t.Fatalf("status must not list ignored files:\n%s", got)
	}
	if !strings.Contains(got, "inspect before overwriting") {
		t.Fatalf("status should warn the model to inspect first:\n%s", got)
	}

	plain := t.TempDir()
	if got := directWorkspaceStatus(plain); got != "" {
		t.Fatalf("non-git workspace should have no status block, got %q", got)
	}
}
