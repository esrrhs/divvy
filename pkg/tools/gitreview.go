package tools

import (
	"context"
	"fmt"
	"os"
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
	// reviewMaxUntrackedBytes skips huge untracked files during the
	// pre-finish scan (binaries and generated dumps are not text-reviewed).
	reviewMaxUntrackedBytes = 256 * 1024
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

// reviewFinding is one deterministic issue reported by the review.
type reviewFinding struct {
	kind   string // conflict | secret | debug | size
	where  string // "file.go:12" or "" for diff-wide notes
	detail string
}

// reviewResult aggregates findings from tracked diffs and untracked files.
type reviewResult struct {
	findings []reviewFinding
	totals   map[string]int
	files    map[string]bool
	added    int
	deleted  int
}

func newReviewResult() *reviewResult {
	return &reviewResult{totals: map[string]int{}, files: map[string]bool{}}
}

// add records a finding, keeping only reviewMaxFindingsPerKind shown ones
// per kind (totals still reflect every hit).
func (r *reviewResult) add(f reviewFinding) {
	r.totals[f.kind]++
	if r.totals[f.kind] <= reviewMaxFindingsPerKind {
		r.findings = append(r.findings, f)
	}
}

// changed returns added+deleted line count.
func (r *reviewResult) changed() int { return r.added + r.deleted }

// has reports whether any finding of a kind exists (beyond the shown cap).
func (r *reviewResult) has(kind string) bool { return r.totals[kind] > 0 }

// scanTextLine applies the per-line checks and records findings anchored at
// "file:line". Used for both added diff lines and whole untracked files.
func (r *reviewResult) scanTextLine(file, content string, line int) {
	where := fmt.Sprintf("%s:%d", file, line)
	for _, re := range reviewConflictMarks {
		if re.MatchString(strings.TrimSpace(content)) {
			r.add(reviewFinding{"conflict", where, "unresolved merge conflict marker"})
			break
		}
	}
	for _, re := range reviewSecretPatterns {
		if m := re.FindString(content); m != "" {
			r.add(reviewFinding{"secret", where, "possible hard-coded secret: " + clipReviewText(m)})
			break
		}
	}
	for _, re := range reviewDebugPatterns {
		if m := re.FindString(content); m != "" {
			r.add(reviewFinding{"debug", where, "leftover debug statement: " + clipReviewText(strings.TrimSpace(m))})
			break
		}
	}
}

// parseTrackedDiff walks a `git diff --unified=0` body, tracking new-file
// line numbers per hunk and checking added lines only.
func (r *reviewResult) parseTrackedDiff(diff string) {
	file := ""
	newLine := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ b/"):
			file = strings.TrimPrefix(raw, "+++ b/")
			r.files[file] = true
		case strings.HasPrefix(raw, "+++ /dev/null"):
			// Deleted file: it contributes no new lines.
			file = ""
		case strings.HasPrefix(raw, "@@"):
			if m := reviewHunkStart.FindStringSubmatch(raw); m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "+++"):
			r.added++
			if file != "" {
				newLine++
				r.scanTextLine(file, strings.TrimPrefix(raw, "+"), newLine)
			}
		case strings.HasPrefix(raw, "-") && !strings.HasPrefix(raw, "---"):
			r.deleted++
		}
	}
}

// scanUntracked reads newly created, gitignored-excluded files (git diff
// never covers them) and reviews their full contents as added text.
func (r *reviewResult) scanUntracked(sb *Sandbox, paths []string) {
	for _, rel := range paths {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs, err := sb.Resolve(rel)
		if err != nil {
			continue
		}
		// Skip directories and huge files; ReadFile below also rejects
		// binary/non-UTF-8 content.
		if st, statErr := os.Stat(abs); statErr != nil || st.IsDir() || st.Size() > reviewMaxUntrackedBytes {
			continue
		}
		content, err := sb.ReadFile(rel)
		if err != nil {
			continue // binary / non-UTF-8 / unreadable: not text-reviewed
		}
		r.files[rel] = true
		for i, line := range strings.Split(content, "\n") {
			if line != "" {
				r.added++
			}
			r.scanTextLine(rel, line, i+1)
		}
	}
}

// addSizeWarning appends the diff-wide size note when the change is large.
func (r *reviewResult) addSizeWarning() {
	if r.changed() > reviewSizeWarnLines {
		r.add(reviewFinding{
			kind: "size",
			detail: fmt.Sprintf("large diff: %d changed line(s) (+%d -%d) across %d file(s); verify carefully or split it",
				r.changed(), r.added, r.deleted, len(r.files)),
		})
	}
}

// cleanText summarizes a finding-free change (same wording as the historical
// review_diff clean report).
func (r *reviewResult) cleanText() string {
	return fmt.Sprintf(
		"clean (%d changed line(s) (+%d -%d) across %d file(s); no conflict markers, debug leftovers, or hard-coded secrets)",
		r.changed(), r.added, r.deleted, len(r.files))
}

// issuesText formats findings; returns "" when there are none. The size
// warning, when present, is included as a non-blocking finding.
func (r *reviewResult) issuesText() string {
	r.addSizeWarning()
	if len(r.findings) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d issue(s) in %d changed line(s) (+%d -%d) across %d file(s)\n",
		len(r.findings), r.changed(), r.added, r.deleted, len(r.files))
	for _, f := range r.findings {
		if f.where != "" {
			fmt.Fprintf(&b, "[%s] %s - %s\n", f.kind, f.where, f.detail)
		} else {
			fmt.Fprintf(&b, "[%s] %s\n", f.kind, f.detail)
		}
	}
	for _, kind := range []string{"conflict", "secret", "debug"} {
		if more := r.totals[kind] - reviewMaxFindingsPerKind; more > 0 {
			fmt.Fprintf(&b, "[%s] %d more finding(s) omitted; run git diff for the full list\n", kind, more)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReviewDiff runs a deterministic, read-only review of the pending git diff
// (working tree by default, staged area with staged=true). It inspects added
// lines only and checks for: unresolved conflict markers, high-confidence
// hard-coded secrets, leftover debug statements, and an oversized diff.
// Untracked files are not part of git diff and are not reviewed here; the
// pre-finish gate uses ReviewWorkingTree to cover them as well.
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

	r := newReviewResult()
	r.parseTrackedDiff(diff)
	if issues := r.issuesText(); issues != "" {
		return "review_diff: " + issues, nil
	}
	return "review_diff: " + r.cleanText(), nil
}

// FinishGate is the pre-finish verdict over the whole working tree.
type FinishGate struct {
	// Blocking means conflict markers or secrets were found; the worker must
	// refuse finish and hand the report back to the model. Debug prints and
	// large-diff notes stay warnings.
	Blocking bool
	// Report is the human-readable review text (no "review_diff:" prefix).
	Report string
}

// PreFinishGate reviews tracked changes AND untracked files (new files a
// leaf just created never appear in git diff). It returns (nil, nil) outside
// a repository or when the tree is completely clean; review plumbing errors
// are returned so callers can decide whether to fail open.
func (s *Sandbox) PreFinishGate(ctx context.Context) (*FinishGate, error) {
	if !IsRepo(s.Root) {
		return nil, nil
	}
	diff, err := s.git(ctx, "--no-pager", "diff", "--no-color", "--unified=0")
	if err != nil {
		return nil, err
	}
	untracked, err := s.git(ctx, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	trackedEmpty := strings.TrimSpace(diff) == "" || strings.Contains(diff, "(clean/empty)")
	var paths []string
	if strings.TrimSpace(untracked) != "" && !strings.Contains(untracked, "(clean/empty)") {
		paths = strings.Split(strings.TrimSpace(untracked), "\n")
	}
	if trackedEmpty && len(paths) == 0 {
		return nil, nil
	}

	r := newReviewResult()
	if !trackedEmpty {
		r.parseTrackedDiff(diff)
	}
	r.scanUntracked(s, paths)

	report := r.issuesText()
	if report == "" {
		return nil, nil
	}
	blocking := r.has("conflict") || r.has("secret")
	return &FinishGate{Blocking: blocking, Report: report}, nil
}

func clipReviewText(s string) string {
	const max = 80
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
