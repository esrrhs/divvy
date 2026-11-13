package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=divvy",
		"GIT_AUTHOR_EMAIL=divvy@example.invalid",
		"GIT_COMMITTER_NAME=divvy",
		"GIT_COMMITTER_EMAIL=divvy@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTestFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestSearchRespectsGitignore keeps retrieval (agent search, web project
// search, find_files) aligned with git: build output, local secrets and
// generated logs that the project already ignores must not drown the results.
// read_file stays unfiltered because the model may still need to inspect an
// ignored file explicitly.
func TestSearchRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGitTest(t, root, "init", "-q")
	writeTestFile(t, root, ".gitignore", "secret/\n*.log\n!keep.log\n")
	writeTestFile(t, root, "app.go", "package app // needle\n")
	writeTestFile(t, root, "notes.txt", "needle here\n")
	writeTestFile(t, root, "secret/hidden.go", "package secret // needle\n")
	writeTestFile(t, root, "debug.log", "needle in a log\n")
	writeTestFile(t, root, "keep.log", "needle in a kept log\n")

	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sb.SearchFiles("needle", ".", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app.go", "notes.txt", "keep.log"} {
		if !strings.Contains(out, want) {
			t.Fatalf("search should include non-ignored %s:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"secret/hidden.go", "debug.log"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("search must skip ignored %s:\n%s", unwanted, out)
		}
	}

	found, err := sb.FindFiles("*.go", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found, "app.go") || strings.Contains(found, "secret/hidden.go") {
		t.Fatalf("find_files must apply the same ignore rules:\n%s", found)
	}

	// Explicit reads still work: ignore rules are for retrieval, not access.
	if body, err := sb.ReadFile("secret/hidden.go"); err != nil || !strings.Contains(body, "needle") {
		t.Fatalf("read_file should still open an ignored file, err=%v body=%q", err, body)
	}
}

// TestIgnoredPaths_NoRepoAndNoRules covers the fallbacks: outside git nothing
// is filtered, and a repo without ignore rules costs one no-op check.
func TestIgnoredPaths_NoRepoAndNoRules(t *testing.T) {
	plain, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.IgnoredPaths([]string{"a.go"}); got != nil {
		t.Fatalf("non-repo workspace must not filter, got %v", got)
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGitTest(t, root, "init", "-q")
	writeTestFile(t, root, "a.go", "package a\n")
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := sb.IgnoredPaths([]string{"a.go", "b.go"}); got != nil {
		t.Fatalf("repo without ignore rules should return nil, got %v", got)
	}
}
