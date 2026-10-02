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
	maxSearchFileSize = 1 << 20 // skip individual files larger than 1MB
)

// SearchFiles walks the workspace and reports regex matches as compact
// "relative/path:line: matched text" lines. It lets a leaf locate a symbol
// without reading whole files, saving steps and context.
//
// pattern is a regular expression. rootDir is workspace-relative ("." by
// default). glob, when set, filters by the slash-relative file path
// (e.g. "*.go" or "pkg/*_test.go"). Search is case-insensitive unless
// caseSensitive is set.
func (s *Sandbox) SearchFiles(pattern, rootDir, glob string, caseSensitive bool) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("search_files needs a pattern")
	}
	flag := ""
	if !caseSensitive {
		flag = "(?i)"
	}
	re, err := regexp.Compile(flag + pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = "."
	}
	absRoot, err := s.Resolve(rootDir)
	if err != nil {
		return "", err
	}

	var matcher func(relSlash string) bool
	if gl := strings.TrimSpace(glob); gl != "" {
		matcher = func(relSlash string) bool {
			ok, _ := path.Match(gl, relSlash)
			if ok {
				return true
			}
			// Also allow a bare pattern like "*.go" to match by basename.
			ok, _ = path.Match(gl, path.Base(relSlash))
			return ok
		}
	}

	var b strings.Builder
	matches := 0
	filesScanned := 0
	truncated := false

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
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matcher != nil && !matcher(relSlash) {
			return nil
		}
		filesScanned++
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if !utf8.Valid(data) {
			return nil
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			matches++
			if matches > defaultMaxMatches {
				truncated = true
				return nil
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", relSlash, lineNo+1, clipLine(line))
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	if matches == 0 {
		return fmt.Sprintf("(no matches for %q in %d files)", pattern, filesScanned), nil
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("[more than %d matches; refine pattern or glob]\n", defaultMaxMatches)
	}
	return out, nil
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

	var b strings.Builder
	found := 0
	truncated := false
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
		found++
		if found > defaultMaxFoundFiles {
			truncated = true
			return nil
		}
		fmt.Fprintf(&b, "%s\n", relSlash)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
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
