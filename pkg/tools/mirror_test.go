package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMirror_SnapshotCopiesNestedAndSkipsIgnored(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a", "b", "deep.txt"), "deep")
	writeFile(t, filepath.Join(src, "top.txt"), "top")
	writeFile(t, filepath.Join(src, ".git", "config"), "ignored")
	writeFile(t, filepath.Join(src, "node_modules", "x.js"), "ignored")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if got := readFile(t, filepath.Join(m.Dir, "a", "b", "deep.txt")); got != "deep" {
		t.Fatalf("nested copy wrong: %q", got)
	}
	if got := readFile(t, filepath.Join(m.Dir, "top.txt")); got != "top" {
		t.Fatalf("top copy wrong: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git should not be copied")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("node_modules should not be copied")
	}
}

func TestMirror_MergeBackModifiedNewDeleted(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "keep.txt"), "unchanged")
	writeFile(t, filepath.Join(src, "edit.txt"), "old")
	writeFile(t, filepath.Join(src, "gone.txt"), "doomed")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// A file created in the source after the snapshot must not be touched.
	writeFile(t, filepath.Join(src, "late.txt"), "from-sibling")

	writeFile(t, filepath.Join(m.Dir, "edit.txt"), "new")
	writeFile(t, filepath.Join(m.Dir, "added", "new.txt"), "fresh")
	if err := os.Remove(filepath.Join(m.Dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	merged, deleted, err := m.MergeBack()
	if err != nil {
		t.Fatal(err)
	}
	assertSet := func(got []string, want ...string) {
		t.Helper()
		set := map[string]bool{}
		for _, s := range got {
			set[s] = true
		}
		if len(set) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for _, w := range want {
			if !set[w] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	assertSet(merged, "edit.txt", "added/new.txt")
	assertSet(deleted, "gone.txt")

	if got := readFile(t, filepath.Join(src, "edit.txt")); got != "new" {
		t.Fatalf("edit not merged: %q", got)
	}
	if got := readFile(t, filepath.Join(src, "added", "new.txt")); got != "fresh" {
		t.Fatalf("new file not merged: %q", got)
	}
	if _, err := os.Stat(filepath.Join(src, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("deletion not propagated")
	}
	if got := readFile(t, filepath.Join(src, "keep.txt")); got != "unchanged" {
		t.Fatalf("unchanged file modified: %q", got)
	}
	if got := readFile(t, filepath.Join(src, "late.txt")); got != "from-sibling" {
		t.Fatalf("post-snapshot file clobbered: %q", got)
	}
}

func TestMirror_MergeBackUnchangedIsEmpty(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "same.txt"), "same")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	merged, deleted, err := m.MergeBack()
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 0 || len(deleted) != 0 {
		t.Fatalf("expected no changes, got merged=%v deleted=%v", merged, deleted)
	}
	m.Close()
	if _, err := os.Stat(m.Dir); !os.IsNotExist(err) {
		t.Fatal("Close did not remove mirror")
	}
}

func TestMirror_SandboxConfined(t *testing.T) {
	src := t.TempDir()
	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	sb, err := m.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	if sb.Root != m.Dir {
		t.Fatalf("sandbox root %s, want mirror %s", sb.Root, m.Dir)
	}
	if err := sb.WriteFile("x.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped the mirror")
	}
}

func TestHasGoMod(t *testing.T) {
	dir := t.TempDir()
	if HasGoMod(dir) {
		t.Fatal("empty dir should not have go.mod")
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	if !HasGoMod(dir) {
		t.Fatal("go.mod not detected")
	}
}
