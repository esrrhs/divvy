package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// gitAvailable skips worktree tests on machines without git.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// assertWorktreeDeregistered verifies Close both removed the mirror
// directory and deregistered it from the source repository.
func assertWorktreeDeregistered(t *testing.T, repo, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("mirror dir %s still exists after Close", dir)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), dir) {
		t.Fatalf("worktree still registered after Close:\n%s", out)
	}
}

// TestMirror_WorktreeUsedForGitRepo is the core regression: an isolated leaf
// in a git workspace used to run inside a plain .git-less copy, where the
// pre-finish gate silently no-op'd. A worktree mirror is a git working tree.
func TestMirror_WorktreeUsedForGitRepo(t *testing.T) {
	gitAvailable(t)
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})

	m, err := NewMirror(sb.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !m.worktree {
		t.Fatal("git repo with commits must snapshot via git worktree")
	}
	if !IsRepo(m.Dir) {
		t.Fatal("worktree mirror must itself be a git working tree")
	}
}

// TestMirror_WorktreeFinishGateActive proves the finish gate works INSIDE an
// isolated leaf: a hard-coded secret in a new file and a conflict marker in
// a tracked file must both block finish.
func TestMirror_WorktreeFinishGateActive(t *testing.T) {
	gitAvailable(t)
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})

	m, err := NewMirror(sb.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	msb, err := m.Sandbox()
	if err != nil {
		t.Fatal(err)
	}

	// New untracked file with a hard-coded AWS key.
	if err := msb.WriteFile("leak.go", "package main\n\nvar awsKey = \"AKIAIOSFODNN7EXAMPLE\"\n"); err != nil {
		t.Fatal(err)
	}
	// Tracked file with an unresolved conflict marker.
	if err := msb.WriteFile("main.go", "package main\n\nfunc main() {\n<<<<<<< HEAD\nx()\n=======\ny()\n>>>>>>> b\n}\n"); err != nil {
		t.Fatal(err)
	}

	gate, err := msb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gate == nil || !gate.Blocking {
		t.Fatalf("finish gate must block secret + conflict marker in worktree: %+v", gate)
	}
	if !strings.Contains(gate.Report, "[secret] leak.go:") {
		t.Fatalf("report missing untracked-file secret:\n%s", gate.Report)
	}
	if !strings.Contains(gate.Report, "[conflict] main.go:") {
		t.Fatalf("report missing tracked-file conflict marker:\n%s", gate.Report)
	}
}

// TestMirror_WorktreeOverlaysDirtyState verifies a later leaf sees earlier
// leaves' merged-but-uncommitted work: tracked modifications and deletions
// (vs HEAD) and untracked files must be present in the new worktree, while
// internal directories such as .divvy stay out.
func TestMirror_WorktreeOverlaysDirtyState(t *testing.T) {
	gitAvailable(t)
	sb := setupGitRepo(t, map[string]string{
		"tracked.txt": "v1\n",
		"doomed.txt":  "x\n",
	})

	// Make the source working tree dirty without committing.
	if err := sb.WriteFile("tracked.txt", "v2\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sb.Root, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sb.Root, "untracked.txt"), "fresh\n")
	writeFile(t, filepath.Join(sb.Root, ".divvy", "s.json"), "internal\n")

	m, err := NewMirror(sb.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if got := readFile(t, filepath.Join(m.Dir, "tracked.txt")); got != "v2\n" {
		t.Fatalf("dirty tracked change missing from mirror: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "doomed.txt")); !os.IsNotExist(err) {
		t.Fatal("tracked deletion not reflected in mirror")
	}
	if got := readFile(t, filepath.Join(m.Dir, "untracked.txt")); got != "fresh\n" {
		t.Fatalf("untracked file missing from mirror: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, ".divvy")); !os.IsNotExist(err) {
		t.Fatal(".divvy must not be overlaid into the mirror")
	}

	// Mirror matches the source's live state, so merging it back is a no-op.
	merged, deleted, err := m.MergeBack()
	if err != nil {
		t.Fatalf("identical state should merge cleanly: %v", err)
	}
	if len(merged) != 0 || len(deleted) != 0 {
		t.Fatalf("expected no merge output, got merged=%v deleted=%v", merged, deleted)
	}
}

// TestMirror_WorktreeMergeConflict keeps the stale-overwrite guard working
// on worktree mirrors: a sibling edit after snapshot must abort the merge.
func TestMirror_WorktreeMergeConflict(t *testing.T) {
	gitAvailable(t)
	sb := setupGitRepo(t, map[string]string{"f.txt": "old\n"})

	m, err := NewMirror(sb.Root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "f.txt")); got != "old\n" {
		t.Fatalf("baseline: %q", got)
	}

	// Sibling merges after our snapshot; our leaf edits the same file.
	writeFile(t, filepath.Join(sb.Root, "f.txt"), "sibling\n")
	writeFile(t, filepath.Join(m.Dir, "f.txt"), "mine\n")

	_, _, err = m.MergeBack()
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want *MergeConflictError, got %v", err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "f.txt")); got != "sibling\n" {
		t.Fatalf("sibling content clobbered: %q", got)
	}

	m.Close()
	assertWorktreeDeregistered(t, sb.Root, m.Dir)
}

// TestMirror_PreviewChangesNoApply verifies the review payload describes
// added/modified/deleted files with contents WITHOUT touching the source,
// for BOTH mirror backends: a plain copy (non-git workspace) and a linked
// git worktree (git workspace). TR-2.4.
func TestMirror_PreviewChangesNoApply(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (src string)
	}{
		{
			name: "copy",
			setup: func(t *testing.T) string {
				src := t.TempDir()
				writeFile(t, filepath.Join(src, "edit.txt"), "old\n")
				writeFile(t, filepath.Join(src, "gone.txt"), "doomed\n")
				writeFile(t, filepath.Join(src, "keep.txt"), "same\n")
				return src
			},
		},
		{
			name: "worktree",
			setup: func(t *testing.T) string {
				gitAvailable(t)
				return setupGitRepo(t, map[string]string{
					"edit.txt": "old\n",
					"gone.txt": "doomed\n",
					"keep.txt": "same\n",
				}).Root
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.setup(t)

			m, err := NewMirror(src)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if tc.name == "worktree" && !m.worktree {
				t.Fatal("git workspace must produce a worktree mirror")
			}

			writeFile(t, filepath.Join(m.Dir, "edit.txt"), "new\n")
			writeFile(t, filepath.Join(m.Dir, "added", "new.txt"), "fresh\n")
			if err := os.Remove(filepath.Join(m.Dir, "gone.txt")); err != nil {
				t.Fatal(err)
			}

			changes, err := m.PreviewChanges()
			if err != nil {
				t.Fatal(err)
			}
			byPath := map[string]FileChange{}
			for _, c := range changes {
				byPath[c.Path] = c
			}
			if len(byPath) != 3 {
				t.Fatalf("want 3 changes (edit/add/delete), got %+v", changes)
			}
			if c := byPath["edit.txt"]; c.Status != ChangeModified || c.OldContent != "old\n" || c.NewContent != "new\n" || c.Conflict {
				t.Fatalf("edit wrong: %+v", c)
			}
			if c := byPath["added/new.txt"]; c.Status != ChangeAdded || c.OldContent != "" || c.NewContent != "fresh\n" {
				t.Fatalf("add wrong: %+v", c)
			}
			if c := byPath["gone.txt"]; c.Status != ChangeDeleted || c.OldContent != "doomed\n" || c.NewContent != "" {
				t.Fatalf("delete wrong: %+v", c)
			}

			// Preview must not mutate anything: MergeBack afterwards still works.
			if _, _, err := m.MergeBack(); err != nil {
				t.Fatalf("preview poisoned the merge: %v", err)
			}
			if got := readFile(t, filepath.Join(src, "edit.txt")); got != "new\n" {
				t.Fatalf("merge content wrong: %q", got)
			}
			if _, err := os.Stat(filepath.Join(src, "gone.txt")); !os.IsNotExist(err) {
				t.Fatalf("deleted file still present after merge: %v", err)
			}

			if tc.name == "worktree" {
				dir := m.Dir
				m.Close()
				assertWorktreeDeregistered(t, src, dir)
			}
		})
	}
}

// TestMirror_PreviewChangesFlagsSiblingConflict covers a file the leaf
// modified AND a sibling changed after the snapshot.
func TestMirror_PreviewChangesFlagsSiblingConflict(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "base\n")
	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	writeFile(t, filepath.Join(m.Dir, "f.txt"), "mine\n")
	writeFile(t, filepath.Join(src, "f.txt"), "sibling\n")

	changes, err := m.PreviewChanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Conflict || changes[0].Status != ChangeModified {
		t.Fatalf("conflicting change not flagged: %+v", changes)
	}
}

// TestMirror_PreviewChangesBinaryFlagged verifies NUL-containing content is
// marked binary rather than shipped as text.
func TestMirror_PreviewChangesBinaryFlagged(t *testing.T) {
	src := t.TempDir()
	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := writeBinary(filepath.Join(m.Dir, "blob.bin"), []byte{'P', 'K', 0x03, 0x04, 0x00}); err != nil {
		t.Fatal(err)
	}
	changes, err := m.PreviewChanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Binary || changes[0].NewContent != "" {
		t.Fatalf("binary change wrong: %+v", changes)
	}
}

// TestMirror_MergeBackPreservesExecutableBit verifies a chmod +x inside the
// mirror survives the merge.
func TestMirror_MergeBackPreservesExecutableBit(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho hi\n")
	m, err := NewMirror(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	target := filepath.Join(m.Dir, "run.sh")
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MergeBack(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(src, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable bit lost on merge: %v", info.Mode().Perm())
	}
}

// TestMirror_RepoWithoutCommitsFallsBack covers an initialized repository
// with no HEAD: worktree add cannot check anything out, so the copy path is
// used (matching the old, always-working behavior).
func TestMirror_RepoWithoutCommitsFallsBack(t *testing.T) {
	gitAvailable(t)
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(root, "only.txt"), "x\n")

	m, err := NewMirror(root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.worktree {
		t.Fatal("repo with no commits must use the copy fallback")
	}
	if got := readFile(t, filepath.Join(m.Dir, "only.txt")); got != "x\n" {
		t.Fatalf("file missing in copy mirror: %q", got)
	}
}
