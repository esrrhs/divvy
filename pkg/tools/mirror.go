package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// worktreeMu serializes git worktree admin operations (add/remove). Git
// itself locks the worktrees admin directory, but older versions and slow
// disks make concurrent admin commands flaky; one process-wide lock keeps
// mirror setup/teardown deterministic.
var worktreeMu sync.Mutex

// Mirror is a disposable copy of a workspace for isolated leaf execution.
// A leaf works inside the mirror; its changes reach the real workspace only
// via MergeBack (called after verification passes), so a failed attempt can
// never pollute the shared workspace or sibling leaves.
type Mirror struct {
	Dir string // mirror root (a temp directory)
	// base maps each slash-separated relative path present at snapshot time
	// to the sha256 of its contents. MergeBack compares against these digests
	// to tell "this leaf changed the file" apart from "a sibling leaf changed
	// it after the snapshot" — without that distinction a stale mirror
	// silently overwrites a sibling's already-verified work.
	base map[string]string
	src  string

	// worktree marks a mirror created with `git worktree add` rather than a
	// plain directory copy. Such a mirror IS a git working tree, so the
	// pre-finish gate, review_diff and the read-only git tools work inside
	// it. Close must deregister it with git instead of only deleting the
	// directory.
	worktree bool
}

// NewMirror snapshots the workspace at src into a fresh temp directory and
// records a content digest for every file present at snapshot time.
//
// When src is a git repository with at least one commit, the snapshot is a
// detached `git worktree`: it shares the source's object store (fast, no
// .git copy) and is itself a git working tree, which keeps the finish gate
// and git tooling functional inside isolated leaves. The source working
// tree's uncommitted tracked changes and untracked files are overlaid onto
// the new worktree, since merged sibling work is not necessarily committed.
//
// Non-git workspaces (and repositories with no commits) fall back to a plain
// directory copy that skips ignored directories (.git, node_modules, ...)
// and non-regular files (symlinks).
func NewMirror(src string) (*Mirror, error) {
	dir, err := os.MkdirTemp("", "divvy_mirror-")
	if err != nil {
		return nil, fmt.Errorf("create mirror dir: %w", err)
	}
	if repoReadyForWorktree(src) {
		m, werr := newWorktreeMirror(src, dir)
		if werr != nil {
			os.RemoveAll(dir)
			return nil, werr
		}
		return m, nil
	}
	base, err := copyTree(src, dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return &Mirror{Dir: dir, base: base, src: src}, nil
}

// repoReadyForWorktree reports whether src is a git repository whose HEAD
// exists: `git worktree add --detach` needs a commit to check out, so a
// freshly initialized repository with no commits must use the copy fallback.
func repoReadyForWorktree(src string) bool {
	if !IsRepo(src) {
		return false
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "-q", "--verify", "HEAD").Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

// newWorktreeMirror creates a detached git worktree at dir carrying the
// source working tree's current state (HEAD plus uncommitted tracked changes
// and untracked files), then digests its contents for MergeBack.
func newWorktreeMirror(src, dir string) (*Mirror, error) {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()

	if out, err := exec.Command("git", "-C", src, "worktree", "add", "--detach", "-q", dir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	cleanup := func() {
		_ = exec.Command("git", "-C", src, "worktree", "remove", "--force", dir).Run()
		_ = os.RemoveAll(dir)
	}
	if err := overlayWorktreeState(src, dir); err != nil {
		cleanup()
		return nil, err
	}
	base, err := snapshotDigests(dir)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("digest worktree %s: %w", dir, err)
	}
	return &Mirror{Dir: dir, base: base, src: src, worktree: true}, nil
}

// overlayWorktreeState makes a freshly checked-out HEAD worktree match the
// source's live working tree:
//
//   - tracked modifications and deletions (staged or not) are ported with a
//     binary diff against HEAD applied inside the worktree;
//   - untracked, non-ignored regular files are copied individually (the
//     skipDirNames filter keeps .divvy/.git/node_modules etc. out).
//
// Without this overlay a leaf created after a sibling merged (but
// -git-commit was off) would see stale HEAD content instead of the sibling's
// work.
func overlayWorktreeState(src, wt string) error {
	// `git diff` reads the index, which a concurrent `git add` (leaf
	// publish) may briefly lock; retry that one transient failure.
	var diff []byte
	for attempt := 0; ; attempt++ {
		out, runErr := exec.Command("git", "-C", src, "diff", "--binary", "HEAD").Output()
		if runErr == nil {
			diff = out
			break
		}
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		if attempt >= 3 || !strings.Contains(stderr, "index.lock") {
			return fmt.Errorf("read working-tree diff from %s: %w: %s", src, runErr, strings.TrimSpace(stderr))
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	if len(bytes.TrimSpace(diff)) > 0 {
		cmd := exec.Command("git", "apply", "--whitespace=nowarn")
		cmd.Dir = wt
		cmd.Stdin = bytes.NewReader(diff)
		if out, applyErr := cmd.CombinedOutput(); applyErr != nil {
			return fmt.Errorf("overlay tracked changes onto worktree: %w: %s", applyErr, strings.TrimSpace(string(out)))
		}
	}

	out, err := exec.Command("git", "-C", src, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return fmt.Errorf("list untracked files in %s: %w", src, err)
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || skippedRel(rel) {
			continue
		}
		srcPath := filepath.Join(src, filepath.FromSlash(rel))
		info, statErr := os.Stat(srcPath)
		if statErr != nil || !info.Mode().IsRegular() {
			continue // parity with copyTree: skip symlinks/special files
		}
		dst := filepath.Join(wt, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := copyFile(srcPath, dst); err != nil {
			return fmt.Errorf("overlay untracked %s: %w", rel, err)
		}
	}
	return nil
}

// skippedRel reports whether any path segment of rel is in skipDirNames
// (".git", ".divvy", "node_modules", ...). The filter is segment-based so
// "a/node_modules/b" is skipped at any depth.
func skippedRel(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if skipDirNames[seg] {
			return true
		}
	}
	return false
}

// Sandbox returns a workspace-confined sandbox rooted at the mirror.
func (m *Mirror) Sandbox() (*Sandbox, error) {
	return NewSandbox(m.Dir)
}

// MergeConflictError reports files that a sibling leaf modified after this
// mirror was taken. The merge is abandoned rather than applied: overwriting
// would discard the other leaf's verified work, and the caller is expected to
// retry the leaf against a fresh snapshot that already includes the sibling's
// change.
type MergeConflictError struct {
	Paths []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict on %s: another parallel leaf changed the same file after this leaf's snapshot; "+
		"re-run this leaf against a fresh workspace copy", strings.Join(e.Paths, ", "))
}

// MergeBack copies files that are new or modified in the mirror into the
// source workspace and deletes base files that the mirror removed. Files
// created in the source after the snapshot are left untouched unless the
// mirror created the same path.
//
// The merge is all-or-nothing: if any file this leaf touched was also changed
// in the source since the snapshot, nothing is written and a
// *MergeConflictError naming the paths is returned. It returns
// workspace-relative paths of merged and deleted files on success.
func (m *Mirror) MergeBack() (merged, deleted []string, err error) {
	present := make(map[string]bool)

	// Phase 1: classify every difference without touching the workspace.
	type write struct {
		rel string
		dst string
	}
	var writes []write
	var removals []string
	var conflicts []string

	walkErr := filepath.WalkDir(m.Dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == m.Dir {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipDirNames[d.Name()] || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(m.Dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		present[rel] = true

		mirrorSum, err := fileDigest(p)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		currentSum, err := fileDigest(dstPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if mirrorSum == currentSum {
			// Already identical in the workspace; nothing to merge.
			return nil
		}
		baseSum, existedAtSnapshot := m.base[rel]
		switch {
		case !existedAtSnapshot:
			// New in the mirror. If the source grew the same path since the
			// snapshot, two leaves created it independently.
			if currentSum != "" {
				conflicts = append(conflicts, rel)
			} else {
				writes = append(writes, write{rel: rel, dst: dstPath})
			}
		case currentSum == baseSum:
			// Source untouched since the snapshot: a clean fast-forward.
			writes = append(writes, write{rel: rel, dst: dstPath})
		default:
			// Both this leaf and a sibling changed the file.
			conflicts = append(conflicts, rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("merge mirror %s: %w", m.Dir, walkErr)
	}

	// Deletions propagate only for files that existed at snapshot time, and
	// only when the source still holds the snapshot content.
	for rel, baseSum := range m.base {
		if present[rel] {
			continue
		}
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		currentSum, err := fileDigest(dstPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("check %s: %w", rel, err)
		}
		if currentSum != baseSum {
			conflicts = append(conflicts, rel)
			continue
		}
		removals = append(removals, rel)
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, nil, &MergeConflictError{Paths: conflicts}
	}

	// Phase 2: apply. No conflicts, so nothing below can half-apply.
	sort.Slice(writes, func(i, j int) bool { return writes[i].rel < writes[j].rel })
	for _, w := range writes {
		data, err := os.ReadFile(filepath.Join(m.Dir, filepath.FromSlash(w.rel)))
		if err != nil {
			return nil, nil, fmt.Errorf("read mirrored %s: %w", w.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(w.dst), 0755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(w.dst, data, 0644); err != nil {
			return nil, nil, err
		}
		merged = append(merged, w.rel)
	}
	sort.Strings(removals)
	for _, rel := range removals {
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		if rmErr := os.Remove(dstPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return merged, nil, fmt.Errorf("delete %s: %w", rel, rmErr)
		}
		deleted = append(deleted, rel)
	}
	return merged, deleted, nil
}

// Close removes the mirror. A worktree mirror is deregistered with git
// first (plainly deleting its directory would leave an administrative entry
// under the source's .git/worktrees); if git refuses (e.g. the worktree was
// partially torn down), the directory is removed anyway and stale worktree
// metadata pruned.
func (m *Mirror) Close() {
	if m == nil || m.Dir == "" {
		return
	}
	if !m.worktree {
		os.RemoveAll(m.Dir)
		return
	}
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	if err := exec.Command("git", "-C", m.src, "worktree", "remove", "--force", m.Dir).Run(); err != nil {
		_ = os.RemoveAll(m.Dir)
		_, _ = exec.Command("git", "-C", m.src, "worktree", "prune").Output()
	}
}

// fileDigest returns the hex sha256 of a file's contents, or "" when the file
// does not exist.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyTree copies the file tree at src into dst, returning a map of copied
// files to the sha256 of their snapshot contents.
func copyTree(src, dst string) (map[string]string, error) {
	copied := make(map[string]string)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == src {
			return os.MkdirAll(dst, 0755)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0755)
		}
		if skipDirNames[d.Name()] || !d.Type().IsRegular() {
			return nil
		}
		if err := copyFile(p, filepath.Join(dst, rel)); err != nil {
			return err
		}
		sum, err := fileDigest(p)
		if err != nil {
			return err
		}
		copied[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	return copied, nil
}

// snapshotDigests maps every regular, non-ignored file under root to the
// sha256 of its contents. It is the worktree counterpart of the digest map
// copyTree builds for copy mirrors: in a linked worktree ".git" is a regular
// file (not a directory), and it must be skipped just like the copy path
// skips the .git directory.
func snapshotDigests(root string) (map[string]string, error) {
	digests := make(map[string]string)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipDirNames[d.Name()] || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum, err := fileDigest(p)
		if err != nil {
			return err
		}
		digests[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	return digests, nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// HasGoMod reports whether go.mod exists under root.
func HasGoMod(root string) bool {
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil
}

// TrimPath renders a path for prompts, abbreviating temp mirrors.
func TrimPath(p string) string {
	if i := strings.Index(p, "divvy_mirror-"); i > 0 {
		return p[:i] + "(isolated workspace)"
	}
	return p
}
