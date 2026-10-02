package agent

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/esrrhs/go_llm_engine/pkg/tools"
)

func sanitizeID(title string) string {
	title = strings.TrimSpace(strings.ToLower(title))
	var b strings.Builder
	prevUS := false
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUS = false
		case r == '_' || r == '-':
			if !prevUS && b.Len() > 0 {
				b.WriteByte('_')
				prevUS = true
			}
		case unicode.IsSpace(r) || r == '/' || r == '.':
			if !prevUS && b.Len() > 0 {
				b.WriteByte('_')
				prevUS = true
			}
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		out = "task"
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "_-")
	}
	return out
}

func uniqueID(base string, taken func(string) bool) string {
	if base == "" || base == "root" {
		base = "task"
	}
	if !taken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		id := fmt.Sprintf("%s_%d", base, i)
		if !taken(id) {
			return id
		}
	}
	return fmt.Sprintf("%s_%d", base, 1000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func looksLikePath(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return false
	}
	return strings.Contains(s, "/") || strings.Contains(s, ".") || strings.HasSuffix(s, ".go")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// pythonCheckCmd byte-compiles every Python file and, unlike compileall's
// always-zero exit, fails (via py_compile) on syntax errors. It is a no-op
// when no .py files exist. __pycache__/.git are excluded.
const pythonCheckCmd = `files=$(find . -name "*.py" -not -path "*/__pycache__/*" -not -path "./.git/*"); test -z "$files" || python3 -m py_compile $files`

func defaultDoDCommands(outputs []string, proj tools.ProjectType) []string {
	cmds := make([]string, 0, len(outputs)+2)
	for _, out := range outputs {
		if looksLikePath(out) {
			cmds = append(cmds, "test -f "+shellQuote(out))
		}
	}
	// Language-level checks. These defaults are deliberately conservative:
	// they must be present in a normal toolchain and succeed on valid code.
	// The model's own DoD commands always take precedence over these.
	switch proj {
	case tools.ProjectGo:
		cmds = append(cmds, "go test ./...")
	case tools.ProjectNode:
		// --if-present: exit 0 when the package defines no test script,
		// rather than failing with npm's "no test specified" error.
		cmds = append(cmds, "npm test --if-present")
	case tools.ProjectRust:
		cmds = append(cmds, "cargo test")
	case tools.ProjectPython:
		// Syntax check with real non-zero exit on broken code.
		cmds = append(cmds, pythonCheckCmd)
	}
	if len(cmds) == 0 {
		cmds = []string{"ls"}
	}
	return cmds
}
