package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// reviewSizeWarnLines flags a diff whose added+deleted line count is large
	// enough that a weak model should double-check it (or split the change).
	reviewSizeWarnLines = 600
	// reviewMaxFindingsPerKind keeps each category compact in the prompt.
	reviewMaxFindingsPerKind = 10
)

var (
	// Conflict markers from an unresolved merge/rebase.
	reviewConflictMarks = []*regexp.Regexp{
		regexp.MustCompile(`^<{7}\b`),
		regexp.MustCompile(`^={7}$`),
		regexp.MustCompile(`^>{7}\b`),
		regexp.MustCompile(`^\|{7}\b`),
	}
	// High-confidence secret shapes.
	reviewSecretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),           // AWS access key id
		regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}`), // Slack token
		regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd)\s*[:=]+\s*["'][^"']{6,}["']`),
	}
	// Leftover interactive debugging statements.
	reviewDebugPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bfmt\.Print(?:ln|f)?\s*\(`),
		regexp.MustCompile(`\bprintln\s*\(`),
		regexp.MustCompile(`\bconsole\.log\s*\(`),
		regexp.MustCompile(`\bdebugger\b`),
		regexp.MustCompile(`\bpdb\.set_trace\s*\(`),
		regexp.MustCompile(`\bbreakpoint\s*\(`),
	}
	// Hunk header "@@ -a,b +c,d @@" — c is the first new-file line.
	reviewHunkStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// diffFinding is one deterministic issue reported by ReviewDiff.
type diffFinding struct {
	kind   string // conflict | secret | debug | size
	where  string // "file.go:12" or "" for diff-wide notes
	detail string
}

// ReviewDiff runs a deterministic, read-only review of the pending git diff
// (working tree by default, staged area with staged=true). It inspects added
// lines only and checks for: unresolved conflict markers, high-confidence
// hard-coded secrets, leftover debug statements, and an oversized diff.
// Untracked files are not part of git diff and are not reviewed.
func (s *Sandbox) ReviewDiff(ctx context.Context, staged bool) (string, error) {
	if !IsRepo(s.Root) {
		return "", fmt.Errorf("not a git repository: %s", s.Root)
	}
	args := []string{"--no-pager", "diff", "--no-color", "--unified=0"}
	if staged {
		args = append(args, "--cached")
	}
	diff, err := s.git(ctx, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" || strings.Contains(diff, "(clean/empty)") {
		return "review_diff: no tracked changes to review (untracked files are not included)", nil
	}

	var findings []diffFinding
	totals := map[string]int{}
	files := map[string]bool{}
	file := ""
	newLine := 0
	added, deleted := 0, 0

	addFinding := func(f diffFinding) {
		totals[f.kind]++
		if totals[f.kind] <= reviewMaxFindingsPerKind {
			findings = append(findings, f)
		}
	}

	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ b/"):
			file = strings.TrimPrefix(raw, "+++ b/")
			files[file] = true
		case strings.HasPrefix(raw, "+++ /dev/null"):
			// File deleted in the new tree; following added lines belong to
			// nothing (deletes do not produce " +" lines anyway).
			file = ""
		case strings.HasPrefix(raw, "@@"):
			if m := reviewHunkStart.FindStringSubmatch(raw); m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "+++"):
			added++
			if file == "" {
				continue
			}
			newLine++
			content := strings.TrimPrefix(raw, "+")
			for _, re := range reviewConflictMarks {
				if re.MatchString(strings.TrimSpace(content)) {
					addFinding(diffFinding{"conflict", fmt.Sprintf("%s:%d", file, newLine),
						"unresolved merge conflict marker"})
					break
				}
			}
			for _, re := range reviewSecretPatterns {
				if m := re.FindString(content); m != "" {
					addFinding(diffFinding{"secret", fmt.Sprintf("%s:%d", file, newLine),
						"possible hard-coded secret: " + clipReviewText(m)})
					break
				}
			}
			for _, re := range reviewDebugPatterns {
				if m := re.FindString(content); m != "" {
					addFinding(diffFinding{"debug", fmt.Sprintf("%s:%d", file, newLine),
						"leftover debug statement: " + clipReviewText(strings.TrimSpace(m))})
					break
				}
			}
		case strings.HasPrefix(raw, "-") && !strings.HasPrefix(raw, "---"):
			deleted++
		}
	}

	changed := added + deleted
	if changed > reviewSizeWarnLines {
		findings = append(findings, diffFinding{
			kind:   "size",
			detail: fmt.Sprintf("large diff: %d changed line(s) (+%d -%d) across %d file(s); verify carefully or split it", changed, added, deleted, len(files)),
		})
	}

	if len(findings) == 0 {
		return fmt.Sprintf(
			"review_diff: clean (%d changed line(s) (+%d -%d) across %d file(s); no conflict markers, debug leftovers, or hard-coded secrets)",
			changed, added, deleted, len(files)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "review_diff: %d issue(s) in %d changed line(s) (+%d -%d) across %d file(s)\n",
		len(findings), changed, added, deleted, len(files))
	for _, f := range findings {
		if f.where != "" {
			fmt.Fprintf(&b, "[%s] %s - %s\n", f.kind, f.where, f.detail)
		} else {
			fmt.Fprintf(&b, "[%s] %s\n", f.kind, f.detail)
		}
	}
	for _, kind := range []string{"conflict", "secret", "debug"} {
		if more := totals[kind] - reviewMaxFindingsPerKind; more > 0 {
			fmt.Fprintf(&b, "[%s] %d more finding(s) omitted; run git diff for the full list\n", kind, more)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func clipReviewText(s string) string {
	const max = 80
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
