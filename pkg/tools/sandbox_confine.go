package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveLexical maps a user path onto the workspace using path cleaning
// only. It is not sufficient to authorize a host file operation: callers
// must pass the result through confineToRoot so symlink targets cannot
// leave the real workspace.
func (s *Sandbox) resolveLexical(rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	rel = filepath.Clean(rel)
	var candidate string
	if filepath.IsAbs(rel) {
		candidate = filepath.Clean(rel)
	} else {
		candidate = filepath.Clean(filepath.Join(s.Root, rel))
	}
	relToRoot, err := filepath.Rel(s.Root, candidate)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return candidate, nil
}

// Resolve maps a user path onto the workspace and rejects escapes, including
// through symbolic links. The returned path is the confined real location
// (existing paths are fully resolved; missing paths are anchored at the
// real existing parent). Lexical-in-root is not treated as authorization.
func (s *Sandbox) Resolve(rel string) (string, error) {
	lexical, err := s.resolveLexical(rel)
	if err != nil {
		return "", err
	}
	return confineToRoot(s.Root, lexical)
}

// ErrPathEscapesSymlink is returned when a path is lexically inside the
// workspace but follows a symbolic link that leaves it. HTTP handlers map
// this to 403; a generic lexical escape stays a 400.
var ErrPathEscapesSymlink = errors.New("path escapes workspace via symlink")

func mustRealRoot(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			return abs
		}
		return real
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

func isWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(path, root+sep)
}

// confineToRoot ensures path, after following existing symlinks, stays
// inside the real workspace root. This is checked at the moment of the
// call; a later swap of a path component is a residual TOCTOU risk, so
// writers should re-check after creating a file and refuse/remove if the
// final real path escaped.
func confineToRoot(root, path string) (string, error) {
	rootReal := mustRealRoot(root)
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		if abs, aerr := filepath.Abs(real); aerr == nil {
			real = abs
		}
		if !isWithinRoot(real, rootReal) {
			return "", fmt.Errorf("%w: %s", ErrPathEscapesSymlink, path)
		}
		return real, nil
	}
	// Path does not exist (or a component is dangling). Walk up to the
	// nearest existing ancestor and require that ancestor to be inside
	// the real root; then rejoin the missing suffix lexically.
	dir := filepath.Dir(path)
	var missing []string
	missing = append(missing, filepath.Base(path))
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if abs, aerr := filepath.Abs(real); aerr == nil {
				real = abs
			}
			if !isWithinRoot(real, rootReal) {
				return "", fmt.Errorf("%w: %s", ErrPathEscapesSymlink, path)
			}
			out := real
			for i := len(missing) - 1; i >= 0; i-- {
				comp := missing[i]
				if comp == "." || comp == "" {
					continue
				}
				if comp == ".." || strings.ContainsRune(comp, os.PathSeparator) || strings.Contains(comp, "/") {
					return "", fmt.Errorf("path escapes workspace: %s", path)
				}
				out = filepath.Join(out, comp)
			}
			if !isWithinRoot(out, rootReal) {
				return "", fmt.Errorf("%w: %s", ErrPathEscapesSymlink, path)
			}
			return out, nil
		}
		if dir == root || dir == filepath.Dir(dir) {
			return "", fmt.Errorf("path escapes workspace: %s", path)
		}
		missing = append(missing, filepath.Base(dir))
		dir = filepath.Dir(dir)
	}
}

// verifyStillConfined re-evaluates a path after a write and removes the
// file if it now resolves outside the workspace (symlink race).
func (s *Sandbox) verifyStillConfined(abs string) error {
	if _, err := confineToRoot(s.Root, abs); err != nil {
		_ = os.Remove(abs)
		return err
	}
	return nil
}
