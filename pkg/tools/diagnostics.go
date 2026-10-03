package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// maxCompactDiagnostics caps how many recognized error lines get prepended to
// a failed verification output; weak models only need the root causes, not
// the full build log.
const maxCompactDiagnostics = 25

// maxDiagnosticLineLen clips overly long messages (e.g. giant type mismatches)
// so one diagnostic cannot crowd the others out of the prompt.
const maxDiagnosticLineLen = 240

// diagMatcher recognizes file-anchored error lines (which carry the
// location) and status/error-header lines (which carry the verdict or
// exception class) in one toolchain's output.
type diagMatcher struct {
	anchored []*regexp.Regexp
	status   []*regexp.Regexp
}

func (m diagMatcher) matches(line string) bool {
	for _, re := range m.anchored {
		if re.MatchString(line) {
			return true
		}
	}
	for _, re := range m.status {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// diagMatchers maps detected project types to their output patterns. Every
// pattern is intentionally narrow: the block must contain root causes, not
// the whole log.
var diagMatchers = map[ProjectType]diagMatcher{
	ProjectGo: {
		anchored: []*regexp.Regexp{
			// ./x.go:4:2: undefined: nope  (also absolute/indented forms)
			regexp.MustCompile(`^\s*(?:\S*[\\/])?[\w.-]+\.go:\d+(?::\d+)?: .+$`),
		},
		status: []*regexp.Regexp{
			regexp.MustCompile(`^(?:--- FAIL: .*|FAIL(?:\s|$).*|panic: .*)$`),
		},
	},
	ProjectPython: {
		anchored: []*regexp.Regexp{
			//   File "app.py", line 12, in main
			regexp.MustCompile(`^\s*File\s+"[^"]+",\s*line\s+\d+`),
		},
		status: []*regexp.Regexp{
			// NameError: name 'x' is not defined / module.Err: ...
			regexp.MustCompile(`^[A-Za-z_]\w*(?:\.[\w.]+)*(?:Error|Exception|Warning)\b:.*$`),
			// pytest embeds the exception under an "E" prefix:
			// E       AssertionError: assert 1 == 2
			regexp.MustCompile(`^E\s+\S*(?:Error|Exception)\b:.*$`),
			regexp.MustCompile(`^FAILED\s+.+$`),
		},
	},
	ProjectRust: {
		anchored: []*regexp.Regexp{
			//   --> src/main.rs:3:9
			regexp.MustCompile(`^\s*-->\s*\S+:\d+:\d+`),
			// thread '...' panicked at 'msg', src/main.rs:3:9
			regexp.MustCompile(`panicked at\s+.*:\d+:\d+`),
		},
		status: []*regexp.Regexp{
			regexp.MustCompile(`^error(?:\[\w+\])?: .+$`),
			regexp.MustCompile(`^test\s+.+\s+\.\.\.\s*FAILED$`),
		},
	},
	ProjectNode: {
		anchored: []*regexp.Regexp{
			// tsc: src/x.ts(10,5): error TS2304: Cannot find name 'y'.
			regexp.MustCompile(`^\S+\.(?:ts|tsx|js|jsx|mjs|cjs)\(\d+,\d+\):\s*(?:error|note)\b.*$`),
			// Node runtime source-location line: /app/x.js:3 or
			// file:///app/x.mjs:3
			regexp.MustCompile(`^(?:file://)?/\S+\.(?:ts|tsx|js|jsx|mjs|cjs):\d+$`),
		},
		status: []*regexp.Regexp{
			// ReferenceError: y is not defined / SyntaxError: Unexpected token
			regexp.MustCompile(`^[\w.]*Error: .+$`),
		},
	},
}

// CompactDiagnostics extracts the root-cause lines from a failed build/test
// output and renders them as a short block suitable for prepending to the raw
// output (prevError is later head-truncated, so root causes must come first).
// Unsupported toolchains, or output without recognizable diagnostics, return
// "" so callers can prepend unconditionally on a non-empty result.
func CompactDiagnostics(project ProjectType, output string) string {
	matcher, ok := diagMatchers[project]
	if !ok || strings.TrimSpace(output) == "" {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	skipped := 0
	for _, line := range strings.Split(output, "\n") {
		if !matcher.matches(line) {
			continue
		}
		// Normalize whitespace so the same error reached via different
		// indentation/tabs counts once.
		key := strings.Join(strings.Fields(line), " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(lines) >= maxCompactDiagnostics {
			skipped++
			continue
		}
		lines = append(lines, clipDiagnosticLine(line))
	}
	if len(lines) == 0 {
		return ""
	}
	total := len(lines) + skipped
	b := new(strings.Builder)
	fmt.Fprintf(b, "[compact diagnostics: %d compiler/test error line(s); raw output below]\n", total)
	b.WriteString(strings.Join(lines, "\n"))
	if skipped > 0 {
		fmt.Fprintf(b, "\n... %d more diagnostic line(s) omitted; see raw output", skipped)
	}
	b.WriteString("\n---")
	return b.String()
}

// clipDiagnosticLine trims trailing whitespace and caps line length.
func clipDiagnosticLine(line string) string {
	line = strings.TrimRight(line, " \t\r")
	if len(line) <= maxDiagnosticLineLen {
		return line
	}
	return line[:maxDiagnosticLineLen] + " ...[truncated]"
}
