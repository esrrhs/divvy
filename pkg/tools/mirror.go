package tools

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Mirror is a disposable copy of a workspace for isolated leaf execution.
// A leaf works inside the mirror; its changes reach the real workspace only
// via MergeBack (called after verification passes), so a failed attempt can
// never pollute the shared workspace or sibling leaves.
type Mirror struct {
	Dir  string // mirror root (a temp directory)
	base map[string]bool
	src  string
}

// NewMirror copies the workspace at src into a fresh temp directory and
// records which files existed at snapshot time. Skips the usual ignored
// directories (.git, node_modules, ...) and non-regular files (symlinks).
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

// MergeBack copies files that are new or modified in the mirror into the
// source workspace and deletes base files that the mirror removed.
// Files created in the source after the snapshot are left untouched.
// It returns workspace-relative paths of merged and deleted files.
func (m *Mirror) MergeBack() (merged, deleted []string, err error) {
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

		mirrorData, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(m.src, filepath.FromSlash(rel))
		srcData, srcErr := os.ReadFile(dstPath)
		if srcErr == nil && bytes.Equal(mirrorData, srcData) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, mirrorData, 0644); err != nil {
			return err
		}
		merged = append(merged, rel)
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("merge mirror %s: %w", m.Dir, walkErr)
	}

	// Propagate deletions only for files that existed at snapshot time.
	for rel := range m.base {
		if present[rel] {
			continue
		}
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

// copyTree copies the file tree at src into dst, returning the set of copied
// files as slash-separated relative paths.
func copyTree(src, dst string) (map[string]bool, error) {
	copied := make(map[string]bool)
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
		copied[filepath.ToSlash(rel)] = true
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
