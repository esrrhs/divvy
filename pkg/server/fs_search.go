package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/esrrhs/divvy/pkg/tools"
)

// handleFSSearch is a read-only literal (default) or regex search over the
// current project workdir. It reuses Sandbox.SearchFilesHits so the UI and
// the agent tool share ignore rules, size/binary caps, and path confinement.
// Paths in the response are workspace-relative; absolute or escaped paths
// are never returned.
func (s *Server) handleFSSearch(w http.ResponseWriter, r *http.Request) {
	root, ok := s.resolveWorkdir(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q is required"})
		return
	}
	useRegex := queryTruthy(r, "regex")
	caseSensitive := queryTruthy(r, "case")
	glob := strings.TrimSpace(r.URL.Query().Get("glob"))
	limit := queryInt(r, "limit", tools.DefaultMaxMatches())
	if limit <= 0 {
		limit = tools.DefaultMaxMatches()
	}

	pattern := q
	if !useRegex {
		pattern = regexp.QuoteMeta(q)
	}

	sb, err := tools.NewSandbox(root)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	res, err := sb.SearchFilesHits(pattern, ".", glob, caseSensitive, limit)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, tools.ErrPathEscapesSymlink) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	secrets := s.secretsForFileRequest(r)
	hits := make([]tools.SearchHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		if h.Path == "" || strings.HasPrefix(h.Path, "/") ||
			h.Path == ".." || strings.HasPrefix(h.Path, "../") {
			continue
		}
		if _, rerr := sb.Resolve(h.Path); rerr != nil {
			continue
		}
		if len(secrets) > 0 {
			h.Path = Redact(h.Path, secrets...)
			h.Preview = Redact(h.Preview, secrets...)
		}
		h.Preview = tools.ClipSearchPreview(h.Preview)
		hits = append(hits, h)
	}

	query, globOut := q, glob
	if len(secrets) > 0 {
		query = Redact(q, secrets...)
		globOut = Redact(glob, secrets...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"query":          query,
		"literal":        !useRegex,
		"regex":          useRegex,
		"case_sensitive": caseSensitive,
		"glob":           globOut,
		"hits":           hits,
		"truncated":      res.Truncated,
		"scanned":        res.Scanned,
	})
}

func queryTruthy(r *http.Request, key string) bool {
	raw := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(key)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
