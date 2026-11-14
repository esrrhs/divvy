package tools

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// IgnoredPaths reports which workspace-relative paths are excluded by the
// repository's ignore rules. It delegates to `git check-ignore` so nested
// .gitignore files, negations and .git/info/exclude behave exactly like git;
// reimplementing that grammar is a reliable source of subtle bugs.
//
// Only retrieval (search, find, project maps) uses this. read_file and
// list_dir still show ignored files: the model may need to inspect a
// generated or local-only file, and hiding it from the tool would be worse
// than a noisy search result.
func (s *Sandbox) IgnoredPaths(paths []string) map[string]bool {
	if s == nil || len(paths) == 0 || !IsRepo(s.Root) {
		return nil
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	var in bytes.Buffer
	for _, p := range paths {
		p = strings.TrimSpace(filepath.ToSlash(p))
		if p == "" || p == "." || strings.HasPrefix(p, "../") {
			continue
		}
		in.WriteString(p)
		in.WriteByte('\n')
	}
	if in.Len() == 0 {
		return nil
	}
	cmd := exec.Command(git, "-C", s.Root, "check-ignore", "--stdin")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 means "none of these paths are ignored"; any other
		// failure (old git, corrupt index) degrades to no filtering rather
		// than hiding files.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return nil
	}
	ignored := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(filepath.ToSlash(line))
		if line != "" {
			ignored[line] = true
		}
	}
	if len(ignored) == 0 {
		return nil
	}
	return ignored
}
