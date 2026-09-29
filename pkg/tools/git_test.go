package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
}

func TestIsRepo(t *testing.T) {
	dir := t.TempDir()
	if IsRepo(dir) {
		t.Fatal("plain dir should not be a repo")
	}
	initRepo(t, dir)
	if !IsRepo(dir) {
		t.Fatal("initialized dir should be a repo")
	}
	// Nested dirs count too.
	sub := dir + "/a/b"
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(sub) {
		t.Fatal("nested dir should be inside the work tree")
	}
}

func TestCommitAll(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)

	// Nothing to commit on a clean repo.
	committed, hash, err := CommitAll(dir, "empty")
	if err != nil || committed || hash != "" {
		t.Fatalf("clean repo: committed=%v hash=%q err=%v", committed, hash, err)
	}

	if err := os.WriteFile(dir+"/one.txt", []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	committed, hash, err = CommitAll(dir, "first commit")
	if err != nil || !committed || hash == "" {
		t.Fatalf("first commit: committed=%v hash=%q err=%v", committed, hash, err)
	}
	out, err := exec.Command("git", "-C", dir, "log", "--format=%s").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "first commit") {
		t.Fatalf("commit message missing: %q %v", out, err)
	}

	// A second change is picked up by add -A (including new files).
	if err := os.MkdirAll(dir+"/sub", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/sub/two.txt", []byte("2"), 0644); err != nil {
		t.Fatal(err)
	}
	committed, hash2, err := CommitAll(dir, "second commit")
	if err != nil || !committed || hash2 == "" || hash2 == hash {
		t.Fatalf("second commit: committed=%v hash=%q (was %q) err=%v", committed, hash2, hash, err)
	}
}

func TestCommitAllNotARepo(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := CommitAll(dir, "x"); err == nil {
		t.Fatal("expected error outside a repo")
	}
}
