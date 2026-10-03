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

var (
	// goLocationLine matches compiler/vet/test lines anchored at a .go file,
	// relative (./pkg/x.go:3:2: ...), absolute (/a/b/x.go:3:2: ...), or bare
	// indented test assertions (    x_test.go:10: expected ...).
	goLocationLine = regexp.MustCompile(`^\s*(?:\S*[\\/])?[\w.-]+\.go:\d+(?::\d+)?: .+$`)
	// goStatusLine catches test/build verdicts that carry no file location.
	goStatusLine = regexp.MustCompile(`^(?:--- FAIL: .*|FAIL(?:\s|$).*|panic: .*)$`)
)

// CompactDiagnostics extracts the root-cause lines from a failed build/test
// output and renders them as a short block suitable for prepending to the raw
// output (prevError is later head-truncated, so root causes must come first).
// Unsupported toolchains, or output without recognizable diagnostics, return
// "" so callers can prepend unconditionally on a non-empty result.
func CompactDiagnostics(project ProjectType, output string) string {
	if project != ProjectGo || strings.TrimSpace(output) == "" {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	skipped := 0
	for _, line := range strings.Split(output, "\n") {
		if !goLocationLine.MatchString(line) && !goStatusLine.MatchString(line) {
			continue
		}
		// Normalize whitespace so the same error repeated with different
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
