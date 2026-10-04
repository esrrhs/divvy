package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
}

// NewMirror copies the workspace at src into a fresh temp directory and
// records a content digest for every file that existed at snapshot time.
// Skips the usual ignored directories (.git, node_modules, ...) and
// non-regular files (symlinks).
func NewMirror(src string) (*Mirror, error) {
	dir, err := os.MkdirTemp("", "divvy_mirror-")
	if err != nil {
		return nil, fmt.Errorf("create mirror dir: %w", err)
	}
	base, err := copyTree(src, dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return &Mirror{Dir: dir, base: base, src: src}, nil
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

// Close removes the mirror directory.
func (m *Mirror) Close() {
	if m != nil && m.Dir != "" {
		os.RemoveAll(m.Dir)
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
