package server

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Web file browser limits. The browser is read-only and meant for source
// text, so a 1 MiB cap keeps responses cheap and huge/generated files out.
const (
	fsMaxFileBytes   = 1 << 20
	fsBinarySniffLen = 8 * 1024
	fsMaxListEntries = 2000
)

var errPathEscaped = errors.New("path escapes workspace via symlink")

// fsEntry is one directory listing row.
type fsEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"` // "dir" | "file" | "symlink" | "other"
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	Escaped bool   `json:"escaped,omitempty"` // symlink points outside the workdir
}

// resolveWorkdir picks the workspace root for an fs request: the named
// session (live then persisted tree), else the server's default workdir.
func (s *Server) resolveWorkdir(r *http.Request) (string, bool) {
	if sid := r.URL.Query().Get("session"); sid != "" {
		if h := s.mgr.Get(sid); h != nil {
			return h.Workdir(), true
		}
		if storage, err := engine.NewStorage(s.dataDir); err == nil {
			if tree, err := storage.LoadTree(sid); err == nil && tree.WorkDir != "" {
				return tree.WorkDir, true
			}
		}
		return "", false
	}
	return s.workdir, true
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	root, ok := s.resolveWorkdir(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	sb, err := tools.NewSandbox(root)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rel := r.URL.Query().Get("path")
	resolved, err := sb.Resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	real, err := confinedRealPath(sb.Root, resolved)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Lstat(real)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "path not found"})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is not a directory"})
		return
	}

	dirEntries, err := os.ReadDir(real)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot list directory: " + err.Error()})
		return
	}
	out := make([]fsEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		if tools.IsSkippedName(e.Name()) {
			continue
		}
		full := filepath.Join(real, e.Name())
		entryRel := slashRel(sb.Root, resolved, e.Name(), rel)
		fe := fsEntry{Name: e.Name(), Path: entryRel}
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			fe.Type = "symlink"
			if li, err := os.Lstat(full); err == nil {
				fe.Mode = uint32(li.Mode().Perm())
			}
			if target, err := filepath.EvalSymlinks(full); err != nil || !isWithin(target, mustRealRoot(sb.Root)) {
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
	if len(out) > fsMaxListEntries {
		out = out[:fsMaxListEntries]
		truncated = true
	}
	displayRel := strings.TrimPrefix(filepath.ToSlash(rel), "./")
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      displayRel,
		"entries":   out,
		"truncated": truncated,
	})
}

func (s *Server) handleFSFile(w http.ResponseWriter, r *http.Request) {
	root, ok := s.resolveWorkdir(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	sb, err := tools.NewSandbox(root)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rel := r.URL.Query().Get("path")
	if strings.TrimSpace(rel) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	resolved, err := sb.Resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	real, err := confinedRealPath(sb.Root, resolved)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Lstat(real)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	switch {
	case info.IsDir():
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory; use /api/fs/list"})
		return
	case info.Mode()&fs.ModeSymlink != 0:
		// Follow the symlink (its target was proven confined above) and
		// report the target's type/size rather than the link's.
		targetInfo, err := os.Stat(real)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "symlink target not found"})
			return
		}
		if targetInfo.IsDir() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory; use /api/fs/list"})
			return
		}
		if !targetInfo.Mode().IsRegular() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
			return
		}
		info = targetInfo
	case !info.Mode().IsRegular():
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
		return
	}
	if info.Size() > fsMaxFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error":    "file is larger than the 1 MiB read limit",
			"size":     info.Size(),
			"max_size": fsMaxFileBytes,
		})
		return
	}
	data, err := os.ReadFile(real)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if isBinarySniff(data) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": "binary file: text viewing is not supported",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    filepath.ToSlash(rel),
		"size":    len(data),
		"mode":    uint32(info.Mode().Perm()),
		"content": string(data),
	})
}

// confinedRealPath layers symlink confinement on top of Sandbox.Resolve's
// lexical confinement. Callers Lstat first, so resolved normally exists:
// one EvalSymlinks resolves the WHOLE chain (the target itself and every
// ancestor directory), and the result must stay inside the resolved
// workspace root. When the target does not exist, confinement rests on its
// nearest existing ancestor.
func confinedRealPath(root, resolved string) (string, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = root
	}
	if targetReal, err := filepath.EvalSymlinks(resolved); err == nil {
		if !isWithin(targetReal, rootReal) {
			return "", errPathEscaped
		}
		return targetReal, nil
	}
	// Target absent (creation probes are rejected later as 404): validate
	// its existing parent instead.
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(resolved))
	if err != nil || !isWithin(parentReal, rootReal) {
		return "", errPathEscaped
	}
	return resolved, nil
}

// isWithin reports whether path is root itself or below it lexically.
func isWithin(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

func mustRealRoot(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return root
}

// slashRel renders the workspace-relative POSIX path reported to the UI.
func slashRel(root, resolvedDir, name, requestedRel string) string {
	joined := filepath.Join(resolvedDir, name)
	if rel, err := filepath.Rel(root, joined); err == nil {
		return filepath.ToSlash(rel)
	}
	base := strings.TrimSuffix(filepath.ToSlash(requestedRel), "/")
	if base == "" || base == "." {
		return filepath.ToSlash(name)
	}
	return base + "/" + filepath.ToSlash(name)
}

// isBinarySniff applies git's rule to the first chunk: a NUL byte means
// binary.
func isBinarySniff(data []byte) bool {
	n := len(data)
	if n > fsBinarySniffLen {
		n = fsBinarySniffLen
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}
