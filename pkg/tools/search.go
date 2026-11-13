package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxMatches = 100
	maxMatchedLineLen = 200
	maxKeptSearchLine = 16 << 10 // keep enough of a long line for later redaction
	maxSearchFileSize = 1 << 20  // skip individual files larger than 1MB
)

// SearchHit is one content match inside the workspace. Path is always a
// slash-relative path under the sandbox root; it is never an absolute path.
type SearchHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Preview string `json:"preview"`
}

// SearchFilesResult is the structured form of a workspace content search.
type SearchFilesResult struct {
	Hits      []SearchHit `json:"hits"`
	Scanned   int         `json:"scanned"`
	Truncated bool        `json:"truncated"`
}

// SearchFiles walks the workspace and reports regex matches as compact
// "relative/path:line: matched text" lines. It lets a leaf locate a symbol
// without reading whole files, saving steps and context.
//
// pattern is a regular expression. rootDir is workspace-relative ("." by
// default). glob, when set, filters by the slash-relative file path
// (e.g. "*.go" or "pkg/*_test.go"). Search is case-insensitive unless
// caseSensitive is set.
func (s *Sandbox) SearchFiles(pattern, rootDir, glob string, caseSensitive bool) (string, error) {
	res, err := s.SearchFilesHits(pattern, rootDir, glob, caseSensitive, defaultMaxMatches)
	if err != nil {
		return "", err
	}
	if len(res.Hits) == 0 {
		return fmt.Sprintf("(no matches for %q in %d files)", pattern, res.Scanned), nil
	}
	var b strings.Builder
	for _, hit := range res.Hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", hit.Path, hit.Line, ClipSearchPreview(hit.Preview))
	}
	if res.Truncated {
		b.WriteString(fmt.Sprintf("[more than %d matches; refine pattern or glob]\n", defaultMaxMatches))
	}
	return b.String(), nil
}

// SearchFilesHits is the structured counterpart of SearchFiles. The HTTP
// project-search API reuses this so the UI and the agent tool share the
// same sandbox walk, ignore list, size/binary caps, and path confinement.
// limit <= 0 uses defaultMaxMatches; the cap is never raised above that.
func (s *Sandbox) SearchFilesHits(pattern, rootDir, glob string, caseSensitive bool, limit int) (*SearchFilesResult, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("search_files needs a pattern")
	}
	if limit <= 0 || limit > defaultMaxMatches {
		limit = defaultMaxMatches
	}
	flag := ""
	if !caseSensitive {
		flag = "(?i)"
	}
	re, err := regexp.Compile(flag + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = "."
	}
	absRoot, err := s.Resolve(rootDir)
	if err != nil {
		return nil, err
	}

	var matcher func(relSlash string) bool
	if gl := strings.TrimSpace(glob); gl != "" {
		matcher = func(relSlash string) bool {
			ok, _ := path.Match(gl, relSlash)
			if ok {
				return true
			}
			ok, _ = path.Match(gl, path.Base(relSlash))
			return ok
		}
	}

	out := &SearchFilesResult{Hits: []SearchHit{}}
	type candidate struct{ abs, rel string }
	var candidates []candidate
	walkErr := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if p != absRoot && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if p == absRoot || skipDirNames[d.Name()] || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSearchFileSize {
			return nil
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".." || strings.HasPrefix(relSlash, "../") {
			return nil
		}
		// Re-resolve so a walk entry that somehow escaped is dropped
		// rather than returned as an outside path.
		if _, rerr := s.Resolve(relSlash); rerr != nil {
			return nil
		}
		if matcher != nil && !matcher(relSlash) {
			return nil
		}
		candidates = append(candidates, candidate{abs: p, rel: relSlash})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// Filter ignored files after the walk so one `git check-ignore` call
	// covers the whole candidate set instead of spawning git per entry.
	rels := make([]string, 0, len(candidates))
	for _, c := range candidates {
		rels = append(rels, c.rel)
	}
	ignored := s.IgnoredPaths(rels)
	for _, c := range candidates {
		if ignored[c.rel] {
			continue
		}
		out.Scanned++
		data, err := os.ReadFile(c.abs)
		if err != nil {
			continue
		}
		if IsBinarySniff(data) || !utf8.Valid(data) {
			continue
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(out.Hits) >= limit {
				out.Truncated = true
				return out, nil
			}
			out.Hits = append(out.Hits, SearchHit{
				Path:    c.rel,
				Line:    lineNo + 1,
				Preview: keepSearchLine(line),
			})
		}
	}
	return out, nil
}

func DefaultMaxMatches() int { return defaultMaxMatches }

// ClipSearchPreview trims a matched line for display. Callers that mask
// secrets must redact the full kept line first, then clip, so a key that
// crosses the 200-byte cut is not partially exposed.
func ClipSearchPreview(s string) string {
	return clipLine(s)
}

func keepSearchLine(s string) string {
	s = strings.TrimSuffix(s, "\r")
	if len(s) > maxKeptSearchLine {
		return s[:maxKeptSearchLine]
	}
	return s
}

func clipLine(s string) string {
	if len(s) > maxMatchedLineLen {
		return strings.TrimSpace(s[:maxMatchedLineLen]) + "..."
	}
	return strings.TrimSpace(s)
}

const defaultMaxFoundFiles = 200

// FindFiles locates files by glob name pattern (not content) and returns one
// workspace-relative slash path per line. A bare pattern such as "*_test.go"
// matches by basename at every depth; a slash-bearing pattern like
// "pkg/*.go" matches the relative path. Common junk directories (.git,
// node_modules, ...) are skipped. rootDir is workspace-relative ("." by
// default).
func (s *Sandbox) FindFiles(pattern, rootDir string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("find_files needs a pattern")
	}
	if _, err := path.Match(pattern, "x"); err != nil {
		return "", fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = "."
	}
	absRoot, err := s.Resolve(rootDir)
	if err != nil {
		return "", err
	}

	var matched []string
	walkErr := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if p != absRoot && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if p == absRoot || skipDirNames[d.Name()] || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		ok, _ := path.Match(pattern, path.Base(relSlash))
		if !ok {
			ok, _ = path.Match(pattern, relSlash)
		}
		if !ok {
			return nil
		}
		matched = append(matched, relSlash)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	ignored := s.IgnoredPaths(matched)
	var b strings.Builder
	found := 0
	truncated := false
	for _, relSlash := range matched {
		if ignored[relSlash] {
			continue
		}
		found++
		if found > defaultMaxFoundFiles {
			truncated = true
			break
		}
		fmt.Fprintf(&b, "%s\n", relSlash)
	}
	if found == 0 {
		return fmt.Sprintf("(no files matching %q)", pattern), nil
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("[more than %d files; narrow the pattern or path]\n", defaultMaxFoundFiles)
	}
	return out, nil
}
