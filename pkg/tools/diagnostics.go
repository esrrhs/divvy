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

// diagRule is one toolchain's output grammar. Keeping each toolchain's
// knowledge in a single table entry means adding a language is a data change,
// not a control-flow change.
type diagRule struct {
	project ProjectType
	// anchored matches file-anchored error lines, which carry the location
	// and are almost always the root cause.
	anchored []*regexp.Regexp
	// status matches verdict or exception-class lines ("--- FAIL",
	// "error[E0308]:", "ReferenceError:") that name the failure even when
	// the location is on another line.
	status []*regexp.Regexp
}

func (r diagRule) matches(line string) bool {
	for _, re := range r.anchored {
		if re.MatchString(line) {
			return true
		}
	}
	for _, re := range r.status {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// diagRules maps detected project types to their output patterns. Every
// pattern is intentionally narrow: the block must contain root causes, not
// the whole log.
//
// The generic rules at the end are a deliberate fallback for Makefiles and
// unmarked workspaces. Without them a failing `make` produced a full-length
// raw log, which is exactly the context bloat this block exists to prevent.
var diagRules = []diagRule{
	{
		project: ProjectGo,
		anchored: []*regexp.Regexp{
			// ./x.go:4:2: undefined: nope  (also absolute/indented forms)
			regexp.MustCompile(`^\s*(?:\S*[\\/])?[\w.-]+\.go:\d+(?::\d+)?: .+$`),
		},
		status: []*regexp.Regexp{
			regexp.MustCompile(`^(?:--- FAIL: .*|FAIL(?:\s|$).*|panic: .*)$`),
		},
	},
	{
		project: ProjectPython,
		anchored: []*regexp.Regexp{
			//   File "app.py", line 12, in main
			regexp.MustCompile(`^\s*File\s+"[^"]+",\s*line\s+\d+`),
			//   app.py:12: SyntaxError: invalid syntax  (py_compile / pyflakes)
			regexp.MustCompile(`^\s*[\w./\\-]+\.py:\d+(?::\d+)?: .+$`),
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
	{
		project: ProjectRust,
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
	{
		project: ProjectNode,
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
	{
		// Make: the recipe output is arbitrary, but make and its tools still
		// emit recognizable compiler diagnostics, and make's own errors name
		// the failing target.
		project: ProjectMake,
		anchored: []*regexp.Regexp{
			// make: *** [Makefile:12: build] Error 1
			regexp.MustCompile(`^\s*(?:make(?:\[\d+\])?:\s*)?\*\*\*\s+\[.*\]\s+Error\b.*$`),
			// A compiler diagnostic surfaced through a recipe.
			regexp.MustCompile(`^\s*(?:\S*[\\/])?[\w.-]+\.(?:c|cc|cpp|h|java|go|rs|ts|js)\b:[\d:]*\s*(?:error|fatal error)\b.*$`),
		},
		status: []*regexp.Regexp{
			// make: recipe for target 'x' failed
			regexp.MustCompile(`^\s*make(?:\[\d+\])?: \*\*\* .*$`),
			// g++/gcc/clang headline errors.
			regexp.MustCompile(`^\S*(?:error|Error): .+$`),
		},
	},
	{
		// Generic fallback for unmarked workspaces. Deliberately broad: it
		// only fires on lines that clearly name a failure, which is still far
		// better than handing the model an unfiltered build log.
		project: ProjectGeneric,
		anchored: []*regexp.Regexp{
			// path:line: anything, and path(line,col): anything
			regexp.MustCompile(`^\s*\S+:\d+(?::\d+)?[:(]\s*\S.*$`),
		},
		status: []*regexp.Regexp{
			regexp.MustCompile(`^(?:--- FAIL: .*|FAIL(?:\s|$).*|panic: .*|fatal error: .*)$`),
			regexp.MustCompile(`^[\w.]*(?:Error|Exception)\b:.*$`),
			// make-style target failure.
			regexp.MustCompile(`^\s*make(?:\[\d+\])?: \*\*\* .*$`),
		},
	},
}

// diagRuleFor returns the rule set for a project type.
func diagRuleFor(project ProjectType) (diagRule, bool) {
	for _, r := range diagRules {
		if r.project == project {
			return r, true
		}
	}
	return diagRule{}, false
}

// SupportedDiagnosticProjects lists the toolchains CompactDiagnostics
// understands. Callers can use it to explain why a workspace gets no
// compressed block.
func SupportedDiagnosticProjects() []ProjectType {
	out := make([]ProjectType, 0, len(diagRules))
	for _, r := range diagRules {
		out = append(out, r.project)
	}
	return out
}

// CompactDiagnostics extracts the root-cause lines from a failed build/test
// output and renders them as a short block suitable for prepending to the raw
// output (prevError is later head-truncated, so root causes must come first).
// Output without recognizable diagnostics returns "" so callers can prepend
// unconditionally on a non-empty result.
func CompactDiagnostics(project ProjectType, output string) string {
	rule, ok := diagRuleFor(project)
	if !ok || strings.TrimSpace(output) == "" {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	skipped := 0
	for _, line := range strings.Split(output, "\n") {
		if !rule.matches(line) {
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
