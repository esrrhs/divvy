package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/esrrhs/divvy/pkg/tools"
)

// resolveExistingDir requires an already-existing directory and returns its
// cleaned absolute path. It is used both to open a project and to browse.
func resolveExistingDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("directory path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("directory does not exist: %s", abs)
		}
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", abs)
	}
	// Store the canonical directory so conversation workdirs and sandbox
	// roots compare equal even when reached through a symlink (macOS
	// /var -> /private/var, or a symlinked project directory).
	return canonicalPath(abs), nil
}

// canonicalPath resolves a path to its absolute, symlink-free form when it
// exists, and otherwise to its absolute lexical form. Use it whenever paths
// from different surfaces (UI, CLI, sandbox roots, saved sessions) are
// compared or used as map keys.
func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if realAbs, err := filepath.Abs(real); err == nil {
			return realAbs
		}
		return real
	}
	return abs
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		if strings.TrimSpace(s.workdir) != "" && s.workdir != "." {
			raw = s.workdir
		} else if home, err := os.UserHomeDir(); err == nil {
			raw = home
		} else {
			raw = "."
		}
	}
	abs, err := resolveExistingDir(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot list directory: " + err.Error()})
		return
	}

	const maxEntries = 500
	out := make([]fsEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		if tools.IsSkippedName(e.Name()) {
			continue
		}
		// Dot directories other than skipped names stay visible so a user
		// can still open a hidden project folder; regular hidden files are
		// listed but not selectable as a project.
		full := filepath.Join(abs, e.Name())
		fe := fsEntry{Name: e.Name(), Path: full}
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			fe.Type = "symlink"
			if target, err := filepath.EvalSymlinks(full); err == nil {
				if info, err := os.Stat(target); err == nil && info.IsDir() {
					fe.Type = "dir"
					fe.Path = target
				}
			} else {
				fe.Escaped = true
			}
		case e.IsDir():
			fe.Type = "dir"
			if li, err := e.Info(); err == nil {
				fe.Mode = uint32(li.Mode().Perm())
			}
		default:
			if e.Type().IsRegular() {
				fe.Type = "file"
				if li, err := e.Info(); err == nil {
					fe.Size = li.Size()
					fe.Mode = uint32(li.Mode().Perm())
				}
			} else {
				fe.Type = "other"
			}
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type == "dir" && out[j].Type != "dir" {
			return true
		}
		if out[j].Type == "dir" && out[i].Type != "dir" {
			return false
		}
		return out[i].Name < out[j].Name
	})
	truncated := false
	if len(out) > maxEntries {
		out = out[:maxEntries]
		truncated = true
	}

	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      abs,
		"parent":    parent,
		"entries":   out,
		"truncated": truncated,
	})
}
