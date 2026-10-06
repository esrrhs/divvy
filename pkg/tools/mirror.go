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
	// baseMode records the permission bits at snapshot time so a pure
	// chmod (content unchanged) is still detected as a change.
	baseMode map[string]uint32
	src      string

	// worktree marks a mirror created with `git worktree add` rather than a
	// plain directory copy. Such a mirror IS a git working tree, so the
	// pre-finish gate, review_diff and the read-only git tools work inside
	// it. Close must deregister it with git instead of only deleting the
	// directory.
	worktree bool

	// closeOnce makes Close idempotent: rejection/retry paths and defers can
	// both close the same mirror without re-running git worktree admin.
	closeOnce sync.Once
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
	base, baseMode, err := copyTree(src, dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return &Mirror{Dir: dir, base: base, baseMode: baseMode, src: src}, nil
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
	base, baseMode, err := snapshotDigests(dir)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("digest worktree %s: %w", dir, err)
	}
	return &Mirror{Dir: dir, base: base, baseMode: baseMode, src: src, worktree: true}, nil
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

// mirrorWrite is one file the leaf created or modified, pending merge.
type mirrorWrite struct {
	rel string
	dst string
}

// classifyChanges diffs the mirror against its snapshot and the live source
// workspace without touching anything: writes (added/modified, mergeable),
// removals (deleted, mergeable) and conflicts (a sibling changed the same
// path after the snapshot; MergeBack must refuse them).
func (m *Mirror) classifyChanges() (writes []mirrorWrite, removals, conflicts []string, err error) {
	present := make(map[string]bool)

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

		info, err := d.Info()
		if err != nil {
			return err
		}
		mirrorMode := uint32(info.Mode().Perm())
		mirrorSum, err := fileDigest(p)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		currentSum, err := fileDigest(dstPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var currentMode uint32
		if dstInfo, statErr := os.Stat(dstPath); statErr == nil {
			currentMode = uint32(dstInfo.Mode().Perm())
		}
		if mirrorSum == currentSum && mirrorMode == currentMode {
			// Identical in the workspace (contents and permissions); nothing
			// to merge.
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
				writes = append(writes, mirrorWrite{rel: rel, dst: dstPath})
			}
		case currentSum == baseSum && currentMode == m.baseMode[rel]:
			// Source untouched since the snapshot: a clean fast-forward.
			writes = append(writes, mirrorWrite{rel: rel, dst: dstPath})
		default:
			// Both this leaf and a sibling changed the file.
			conflicts = append(conflicts, rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, nil, fmt.Errorf("merge mirror %s: %w", m.Dir, walkErr)
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
			return nil, nil, nil, fmt.Errorf("check %s: %w", rel, err)
		}
		if currentSum != baseSum {
			conflicts = append(conflicts, rel)
			continue
		}
		removals = append(removals, rel)
	}
	sort.Strings(conflicts)
	sort.Slice(writes, func(i, j int) bool { return writes[i].rel < writes[j].rel })
	sort.Strings(removals)
	return writes, removals, conflicts, nil
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
	writes, removals, conflicts, err := m.classifyChanges()
	if err != nil {
		return nil, nil, err
	}
	if len(conflicts) > 0 {
		return nil, nil, &MergeConflictError{Paths: conflicts}
	}

	for _, w := range writes {
		mpath := filepath.Join(m.Dir, filepath.FromSlash(w.rel))
		data, err := os.ReadFile(mpath)
		if err != nil {
			return nil, nil, fmt.Errorf("read mirrored %s: %w", w.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(w.dst), 0755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(w.dst, data, 0644); err != nil {
			return nil, nil, err
		}
		// Preserve the executable bit (and any other permission bits) the
		// leaf set inside the mirror; a verified ./script must stay runnable
		// after the merge.
		if info, statErr := os.Stat(mpath); statErr == nil {
			_ = os.Chmod(w.dst, info.Mode().Perm())
		}
		merged = append(merged, w.rel)
	}
	for _, rel := range removals {
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		if rmErr := os.Remove(dstPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return merged, nil, fmt.Errorf("delete %s: %w", rel, rmErr)
		}
		deleted = append(deleted, rel)
	}
	return merged, deleted, nil
}

// Preview limits for human review payloads: keeps the approval API responsive
// on generated/binary-heavy changes instead of shipping megabytes.
const (
	previewMaxFileBytes  = 256 * 1024
	previewMaxTotalBytes = 4 * 1024 * 1024
)

// FileChangeStatus is the kind of change a leaf made to one path.
type FileChangeStatus string

const (
	ChangeAdded    FileChangeStatus = "added"
	ChangeModified FileChangeStatus = "modified"
	ChangeDeleted  FileChangeStatus = "deleted"
)

// FileChange is one reviewed change in a leaf approval request. OldContent is
// the live source content (for modified/deleted files); NewContent is the
// mirror content (for added/modified files). Binary and oversized files do
// not carry contents. Conflict marks paths a sibling leaf changed after the
// snapshot — approving them cannot merge.
type FileChange struct {
	Path       string           `json:"path"`
	Status     FileChangeStatus `json:"status"`
	Conflict   bool             `json:"conflict,omitempty"`
	Binary     bool             `json:"binary,omitempty"`
	Truncated  bool             `json:"truncated,omitempty"`
	OldMode    uint32           `json:"old_mode,omitempty"`
	NewMode    uint32           `json:"new_mode,omitempty"`
	OldContent string           `json:"old_content,omitempty"`
	NewContent string           `json:"new_content,omitempty"`
}

// looksBinary applies git's heuristic: a NUL byte in the first chunk means
// binary.
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// readPreviewContent loads up to max bytes of a file for review, flagging
// binary content and truncation.
func readPreviewContent(path string, total int) (content string, binary, truncated bool, newTotal int, _ error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, false, total, err
	}
	if looksBinary(data) {
		return "", true, false, total, nil
	}
	if len(data) > previewMaxFileBytes || total+len(data) > previewMaxTotalBytes {
		limit := min(previewMaxFileBytes, max(0, previewMaxTotalBytes-total))
		return string(data[:limit]), false, true, total + limit, nil
	}
	return string(data), false, false, total + len(data), nil
}

// PreviewChanges describes what MergeBack would do, with enough content for a
// human diff review, WITHOUT applying anything. Paths are sorted; conflicts
// are included (flagged) so the UI can explain why approval cannot proceed.
func (m *Mirror) PreviewChanges() ([]FileChange, error) {
	writes, removals, conflicts, err := m.classifyChanges()
	if err != nil {
		return nil, err
	}
	conflictSet := make(map[string]bool, len(conflicts))
	for _, c := range conflicts {
		conflictSet[c] = true
	}

	out := make([]FileChange, 0, len(writes)+len(removals))
	total := 0
	for _, w := range writes {
		fc := FileChange{Path: w.rel, Conflict: conflictSet[w.rel], OldMode: m.baseMode[w.rel]}
		mpath := filepath.Join(m.Dir, filepath.FromSlash(w.rel))
		if info, rerr := os.Stat(mpath); rerr == nil {
			fc.NewMode = uint32(info.Mode().Perm())
		}
		if _, existed := m.base[w.rel]; existed {
			fc.Status = ChangeModified
			if oldData, rerr := os.ReadFile(w.dst); rerr == nil {
				switch {
				case looksBinary(oldData):
					fc.Binary = true
				case len(oldData) > previewMaxFileBytes:
					fc.OldContent = string(oldData[:previewMaxFileBytes])
					fc.Truncated = true
				default:
					fc.OldContent = string(oldData)
				}
			}
		} else {
			fc.Status = ChangeAdded
		}
		content, binary, truncated, newTotal, rerr := readPreviewContent(mpath, total)
		if rerr != nil {
			return nil, rerr
		}
		total = newTotal
		fc.NewContent, fc.Binary, fc.Truncated = content, binary, truncated
		out = append(out, fc)
	}
	for _, rel := range removals {
		fc := FileChange{Path: rel, Status: ChangeDeleted, Conflict: conflictSet[rel], OldMode: m.baseMode[rel]}
		content, binary, truncated, newTotal, rerr := readPreviewContent(filepath.Join(m.src, filepath.FromSlash(rel)), total)
		if rerr != nil && !os.IsNotExist(rerr) {
			return nil, rerr
		}
		total = newTotal
		fc.OldContent, fc.Binary, fc.Truncated = content, binary, truncated
		out = append(out, fc)
	}

	// Conflicting paths that classification found but the writes/removals
	// lists do not carry (they are neither mergeable writes nor removals)
	// still need to be shown.
	covered := make(map[string]bool, len(out))
	for _, fc := range out {
		covered[fc.Path] = true
	}
	for _, c := range conflicts {
		if covered[c] {
			continue
		}
		status := ChangeModified
		if _, existed := m.base[c]; !existed {
			status = ChangeAdded
		}
		out = append(out, FileChange{Path: c, Status: status, Conflict: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
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
	m.closeOnce.Do(func() {
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
	})
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

// copyTree copies the file tree at src into dst, returning maps of copied
// files to the sha256 of their snapshot contents and permission bits.
func copyTree(src, dst string) (map[string]string, map[string]uint32, error) {
	copied := make(map[string]string)
	modes := make(map[string]uint32)
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		copied[key] = sum
		modes[key] = uint32(info.Mode().Perm())
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return copied, modes, nil
}

// snapshotDigests maps every regular, non-ignored file under root to the
// sha256 of its contents and permission bits. It is the worktree counterpart
// of the digest map copyTree builds for copy mirrors: in a linked worktree
// ".git" is a regular file (not a directory), and it must be skipped just
// like the copy path skips the .git directory.
func snapshotDigests(root string) (map[string]string, map[string]uint32, error) {
	digests := make(map[string]string)
	modes := make(map[string]uint32)
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum, err := fileDigest(p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		digests[key] = sum
		modes[key] = uint32(info.Mode().Perm())
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return digests, modes, nil
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
