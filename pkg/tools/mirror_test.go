package tools

import (
	"errors"
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

// TestMirror_MergeBackDetectsStaleOverwrite is the regression test for the
// lost-update defect: two leaves snapshot the same base, both edit the same
// file, and the second merge used to silently overwrite the first leaf's
// already-verified change.
func TestMirror_MergeBackDetectsStaleOverwrite(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "shared.go"), "original\n")

	m1, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m1.Close()
	m2, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	// Leaf 1 edits and merges successfully.
	writeFile(t, filepath.Join(m1.Dir, "shared.go"), "leaf1 version\n")
	if merged, _, err := m1.MergeBack(); err != nil {
		t.Fatalf("leaf1 merge should succeed: %v", err)
	} else if len(merged) != 1 {
		t.Fatalf("leaf1 merged %v, want one file", merged)
	}
	if got := readFile(t, filepath.Join(src, "shared.go")); got != "leaf1 version\n" {
		t.Fatalf("leaf1 not applied: %q", got)
	}

	// Leaf 2's mirror predates leaf1's merge, so it must be rejected.
	writeFile(t, filepath.Join(m2.Dir, "shared.go"), "leaf2 version\n")
	_, _, err = m2.MergeBack()
	if err == nil {
		t.Fatal("stale mirror merge should have been rejected")
	}
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want *MergeConflictError, got %T: %v", err, err)
	}
	if len(conflict.Paths) != 1 || conflict.Paths[0] != "shared.go" {
		t.Fatalf("conflict paths = %v, want [shared.go]", conflict.Paths)
	}
	// The already-verified leaf1 content must survive untouched.
	if got := readFile(t, filepath.Join(src, "shared.go")); got != "leaf1 version\n" {
		t.Fatalf("leaf1 change was clobbered: %q", got)
	}
}

// TestMirror_MergeBackConflictIsAllOrNothing verifies a conflicting merge
// applies none of its writes, leaving no half-applied state behind.
func TestMirror_MergeBackConflictIsAllOrNothing(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "contested.go"), "base\n")
	writeFile(t, filepath.Join(src, "mine.go"), "base\n")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// A sibling merges a change to contested.go after our snapshot.
	writeFile(t, filepath.Join(src, "contested.go"), "sibling version\n")

	// Our leaf changes both files.
	writeFile(t, filepath.Join(m.Dir, "contested.go"), "our version\n")
	writeFile(t, filepath.Join(m.Dir, "mine.go"), "our version\n")

	if _, _, err := m.MergeBack(); err == nil {
		t.Fatal("expected conflict")
	}
	if got := readFile(t, filepath.Join(src, "mine.go")); got != "base\n" {
		t.Fatalf("non-conflicting file was written despite conflict: %q", got)
	}
	if got := readFile(t, filepath.Join(src, "contested.go")); got != "sibling version\n" {
		t.Fatalf("sibling content clobbered: %q", got)
	}
}

// TestMirror_MergeBackDeletionConflict covers a leaf deleting a file that a
// sibling modified after the snapshot.
func TestMirror_MergeBackDeletionConflict(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "victim.go"), "base\n")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := os.Remove(filepath.Join(m.Dir, "victim.go")); err != nil {
		t.Fatal(err)
	}
	// Sibling rewrites the file after our snapshot.
	writeFile(t, filepath.Join(src, "victim.go"), "sibling edit\n")

	_, _, err = m.MergeBack()
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict on deleting a sibling-modified file, got %v", err)
	}
	if got := readFile(t, filepath.Join(src, "victim.go")); got != "sibling edit\n" {
		t.Fatalf("sibling edit lost: %q", got)
	}
}

// TestMirror_MergeBackSamePathCreatedTwice covers two leaves independently
// creating the same new file.
func TestMirror_MergeBackSamePathCreatedTwice(t *testing.T) {
	src := t.TempDir()
	m1, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m1.Close()
	m2, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	writeFile(t, filepath.Join(m1.Dir, "new.go"), "leaf1\n")
	if _, _, err := m1.MergeBack(); err != nil {
		t.Fatalf("leaf1 create should merge: %v", err)
	}
	writeFile(t, filepath.Join(m2.Dir, "new.go"), "leaf2\n")
	_, _, err = m2.MergeBack()
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict when both leaves create the same path, got %v", err)
	}
	if got := readFile(t, filepath.Join(src, "new.go")); got != "leaf1\n" {
		t.Fatalf("first create was clobbered: %q", got)
	}
}

// TestMirror_MergeBackCleanFastForwardStillWorks guards against the conflict
// check being too strict: a leaf that is the only writer must still merge.
func TestMirror_MergeBackCleanFastForwardStillWorks(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.go"), "base\n")
	writeFile(t, filepath.Join(src, "untouched.go"), "base\n")

	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	writeFile(t, filepath.Join(m.Dir, "a.go"), "changed\n")
	merged, deleted, err := m.MergeBack()
	if err != nil {
		t.Fatalf("sole writer should merge cleanly: %v", err)
	}
	if len(merged) != 1 || merged[0] != "a.go" {
		t.Fatalf("merged = %v, want [a.go]", merged)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
	if got := readFile(t, filepath.Join(src, "a.go")); got != "changed\n" {
		t.Fatalf("change not applied: %q", got)
	}
}
