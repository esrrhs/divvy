package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSandbox_WriteFileDoesNotFollowEscapeSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("escape/marker", "escaped"); err == nil {
		t.Fatal("write through symlink leaving the workspace must be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); !os.IsNotExist(err) {
		t.Fatalf("escape marker was written: %v", err)
	}
	// An in-workspace symlink target remains writable.
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("link.txt", "updated"); err != nil {
		t.Fatalf("internal symlink write rejected: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "inside.txt"))
	if err != nil || string(got) != "updated" {
		t.Fatalf("internal write = %q %v", got, err)
	}
}

func TestSandbox_ReadFileDoesNotFollowEscapeSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "sneak")); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile("sneak"); err == nil {
		t.Fatal("read through symlink leaving the workspace must be rejected")
	}
}

func TestMirrorCopyIncludesNodeModulesButMergeSkipsIt(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "node_modules", "auditfixturedep"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "node_modules", "auditfixturedep", "index.js"), []byte("module.exports=1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "app.js"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := os.Stat(filepath.Join(m.Dir, "node_modules", "auditfixturedep", "index.js")); err != nil {
		t.Fatalf("mirror should include project node_modules: %v", err)
	}
	if IsSkippedName("node_modules") != true {
		t.Fatal("search/merge skip list should still hide node_modules")
	}
}
