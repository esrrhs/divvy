package tools

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Why edit anchors tolerate sloppy text
// =====================================
// replace_lines takes an anchor (old_string) and swaps it for new_string. The
// anchor is copied by a model, not by an editor, and small models get three
// things wrong over and over:
//
//  1. they paste the numbered view produced by read_file, so every line starts
//     with "  12: " and nothing in the file does;
//  2. they re-indent the block (tabs vs spaces, an extra level) or drop the
//     trailing whitespace their editor strips on save;
//  3. they quote a fragment that starts or ends mid-line.
//
// Byte-exact matching turns all three into "old_string not found", and a weak
// model answers a not-found with the same anchor again — the loop this project
// exists to avoid. The engine therefore locates the anchor in three passes
// (exact → after dropping read_file line numbers → ignoring horizontal
// whitespace entirely) and only ever applies an edit to a *uniquely* located
// region. Tolerance changes where an edit lands, never whether it is applied
// twice or to the wrong place: any ambiguity is still an error.
//
// The failure message matters as much as the matching. "not found" alone tells
// the model nothing it can act on, so the error quotes the closest region with
// line numbers and the real text — the model can re-copy that instead of
// guessing again.

// anchorSpan is one region of the file where an anchor matched.
type anchorSpan struct {
	start, end int // byte offsets into the LF-normalized text
	firstLine  int // 1-based line of start
	lastLine   int // 1-based line holding the last matched byte
}

// anchorMatch is the outcome of locating an anchor.
type anchorMatch struct {
	spans []anchorSpan
	via   string // "" when byte-exact; otherwise why the loose pass succeeded
}

// maxReportedSpans bounds how many ambiguous occurrences are quoted back.
const maxReportedSpans = 4

// maxAnchorSpans bounds how many occurrences the locator will enumerate. It is
// far above any real edit; hitting it means the anchor is a fragment like "}"
// and the only safe answer is to refuse, because stopping early would leave a
// replace_all half-applied.
const maxAnchorSpans = 65536

// maxHintLines bounds the closest-region hint so a bad anchor cannot flood the
// model's context with a whole function.
const maxHintLines = 8

// anchorLineNumbers matches the "%*d: " prefix read_file puts on every line of
// a windowed read.
var anchorLineNumbers = regexp.MustCompile(`^[ \t]*\d+:[ \t]?`)

// locateAnchor finds every place old matches in text, tolerating the three
// model-side slips listed at the top of this file. It never returns a partial
// guess: a loose match has to be as unambiguous as an exact one.
func locateAnchor(text, old string) (anchorMatch, error) {
	if old == "" {
		return anchorMatch{}, fmt.Errorf("old_string must not be empty")
	}
	lineOf := lineIndexer(text)

	if spans := exactSpans(text, old, lineOf); len(spans) > 0 {
		return anchorMatch{spans: spans}, nil
	}

	var notes []string
	candidate := old
	if stripped, ok := stripAnchorLineNumbers(candidate); ok {
		candidate = stripped
		notes = append(notes, "dropped the read_file line numbers from the anchor")
		if spans := exactSpans(text, candidate, lineOf); len(spans) > 0 {
			return anchorMatch{spans: spans, via: strings.Join(notes, "; ")}, nil
		}
	}

	candidate = trimBlankEdges(candidate)
	if strings.TrimSpace(candidate) == "" {
		return anchorMatch{}, fmt.Errorf("old_string is blank; an anchor has to contain the text to replace")
	}
	if spans := looseSpans(text, candidate, lineOf); len(spans) > 0 {
		notes = append(notes, "matched with whitespace and indentation ignored")
		return anchorMatch{spans: spans, via: strings.Join(notes, "; ")}, nil
	}

	return anchorMatch{}, anchorNotFoundError(text, candidate, notes)
}

// exactSpans lists the non-overlapping byte-exact occurrences of sub.
func exactSpans(text, sub string, lineOf func(int) int) []anchorSpan {
	var spans []anchorSpan
	for off := 0; off <= len(text)-len(sub); {
		i := strings.Index(text[off:], sub)
		if i < 0 {
			break
		}
		s := off + i
		spans = append(spans, makeSpan(s, s+len(sub), lineOf))
		off = s + len(sub)
		if len(spans) >= maxAnchorSpans {
			break
		}
	}
	return spans
}

// makeSpan builds a span and derives its line range from the byte offsets. end
// is exclusive and always > start, so end-1 is a real byte.
func makeSpan(start, end int, lineOf func(int) int) anchorSpan {
	last := end - 1
	if last < start {
		last = start
	}
	return anchorSpan{start: start, end: end, firstLine: lineOf(start), lastLine: lineOf(last)}
}

// looseSpans locates sub in text after removing horizontal whitespace from
// both sides. Positions are mapped back to the original text, so the region
// that gets replaced keeps its real indentation and spacing; only the search
// ignores them.
func looseSpans(text, sub string, lineOf func(int) int) []anchorSpan {
	needle, _, _ := stripHorizontalSpace(sub)
	if len(needle) == 0 {
		return nil
	}
	hay, from, to := stripHorizontalSpace(text)
	var spans []anchorSpan
	for off := 0; off <= len(hay)-len(needle); {
		i := bytes.Index(hay[off:], needle)
		if i < 0 {
			break
		}
		first := off + i
		last := first + len(needle) - 1
		spans = append(spans, makeSpan(from[first], to[last], lineOf))
		off = first + len(needle)
		if len(spans) >= maxAnchorSpans {
			break
		}
	}
	return spans
}

// stripHorizontalSpace drops every space, tab and carriage return and reports
// where each surviving byte came from (from) and the offset just past it (to).
// Newlines are kept: they are structure, not formatting.
func stripHorizontalSpace(s string) (out []byte, from, to []int) {
	out = make([]byte, 0, len(s))
	from = make([]int, 0, len(s))
	to = make([]int, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r':
			continue
		}
		out = append(out, s[i])
		from = append(from, i)
		to = append(to, i+1)
	}
	return out, from, to
}

// stripAnchorLineNumbers removes read_file's "  12: " prefixes when the anchor
// is wholly numbered, which is what a copy-paste of a windowed read looks like.
// It reports false unless every non-blank line carried a prefix.
func stripAnchorLineNumbers(old string) (string, bool) {
	lines := strings.Split(old, "\n")
	numbered, content := 0, 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		content++
		if loc := anchorLineNumbers.FindStringIndex(ln); loc != nil {
			numbered++
		}
	}
	if numbered == 0 || numbered != content {
		return old, false
	}
	for i, ln := range lines {
		lines[i] = anchorLineNumbers.ReplaceAllString(ln, "")
	}
	return strings.Join(lines, "\n"), true
}

// trimBlankEdges drops leading and trailing blank lines, the padding models add
// when they quote a block. Interior blank lines are part of the anchor, and a
// single terminating newline is structure rather than padding: dropping it
// would leave the file's own newline behind and produce a doubled blank line
// once the replacement (which normally ends with "\n") is spliced in.
func trimBlankEdges(s string) string {
	lines := strings.Split(s, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines)
	terminated := end > start && lines[end-1] == ""
	if terminated {
		end--
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if terminated && end > start {
		end++
	}
	if start == 0 && end == len(lines) {
		return s
	}
	return strings.Join(lines[start:end], "\n")
}

// lineIndexer returns a function mapping a byte offset to its 1-based line.
func lineIndexer(text string) func(int) int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return func(off int) int {
		lo, hi := 0, len(starts)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if starts[mid] <= off {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return lo + 1
	}
}

// lineTexts splits text into lines for hint rendering (no terminators).
func lineTexts(text string) []string {
	if text == "" {
		return nil
	}
	out := strings.Split(text, "\n")
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

// anchorAmbiguousError explains a multi-hit anchor and shows where the hits are
// so the model can extend the anchor instead of retrying it verbatim.
func anchorAmbiguousError(path string, text string, spans []anchorSpan, replaceAll bool, via string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "old_string occurs %d times in %s", len(spans), path)
	if via != "" {
		fmt.Fprintf(&b, " (%s)", via)
	}
	if !replaceAll {
		b.WriteString("; pass a larger anchor (include the surrounding lines) or set replace_all")
	}
	b.WriteString(":")
	lines := lineTexts(text)
	for i, sp := range spans {
		if i >= maxReportedSpans {
			fmt.Fprintf(&b, "\n  ... and %d more", len(spans)-maxReportedSpans)
			break
		}
		fmt.Fprintf(&b, "\n  lines %d-%d: %s", sp.firstLine, sp.lastLine, anchorExcerpt(lines, sp, 1))
	}
	return fmt.Errorf("%s", b.String())
}

// anchorExcerpt renders up to n lines of a span for an error message, trimmed
// and clipped so one long generated line cannot dominate the output.
func anchorExcerpt(lines []string, sp anchorSpan, n int) string {
	var parts []string
	for no := sp.firstLine; no <= sp.lastLine && len(parts) < n; no++ {
		if no-1 >= len(lines) {
			break
		}
		parts = append(parts, clipAnchorLine(lines[no-1]))
	}
	return strings.Join(parts, " / ")
}

// clipAnchorLine trims a line and caps it at 100 runes-ish bytes.
func clipAnchorLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 100 {
		s = s[:100] + "..."
	}
	return s
}

// anchorNotFoundError builds the "not found" message: what was tried, then the
// closest region with real line numbers and text. A model that cannot see the
// file text it mis-quoted will mis-quote it again.
func anchorNotFoundError(text, old string, notes []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "old_string not found (%d lines in the file)", len(lineTexts(text)))
	if len(notes) > 0 {
		fmt.Fprintf(&b, "; already tried: %s", strings.Join(notes, "; "))
	}
	if hint := closestRegionHint(text, old); hint != "" {
		b.WriteString("\n")
		b.WriteString(hint)
	} else {
		b.WriteString("\nno region of the file is close to that text; read the file before editing it")
	}
	return fmt.Errorf("%s", b.String())
}

// closestRegionHint finds the window of file lines most similar to the anchor
// and renders it with line numbers, so the model can copy the real text.
func closestRegionHint(text, old string) string {
	fileLines := lineTexts(text)
	oldLines := lineTexts(trimBlankEdges(old))
	if len(fileLines) == 0 || len(oldLines) == 0 {
		return ""
	}
	// Comparing on a whitespace-free form keeps the hint useful when the
	// mismatch *is* the whitespace.
	want := make([][]byte, len(oldLines))
	for i, ln := range oldLines {
		want[i], _, _ = stripHorizontalSpace(ln)
	}
	have := make([][]byte, len(fileLines))
	for i, ln := range fileLines {
		have[i], _, _ = stripHorizontalSpace(ln)
	}

	bestAt, bestScore := -1, 0
	for start := 0; start+len(oldLines) <= len(fileLines); start++ {
		score := 0
		for i := range oldLines {
			if len(want[i]) == 0 {
				continue
			}
			if bytes.Equal(want[i], have[start+i]) {
				score++
			}
		}
		if score > bestScore {
			bestAt, bestScore = start, score
		}
	}
	// A quarter of the lines matching is enough to be worth quoting; below
	// that the hint is noise.
	if bestAt < 0 || bestScore*4 < len(oldLines) {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "closest region, lines %d-%d (%d of %d anchor lines match):",
		bestAt+1, bestAt+len(oldLines), bestScore, len(oldLines))
	shown := 0
	for i := 0; i < len(oldLines) && shown < maxHintLines; i++ {
		fmt.Fprintf(&b, "\n%4d: %s", bestAt+1+i, clipAnchorLine(fileLines[bestAt+i]))
		shown++
	}
	b.WriteString("\ncopy the exact text of those lines (or a smaller unique part of it) as old_string")
	return b.String()
}

// applyAnchorEdit locates the anchor and splices new in its place. When
// replaceAll is false the anchor must match exactly once; otherwise every match
// is replaced, which is the only case where multiple hits are acceptable.
func applyAnchorEdit(text, old, neu string, replaceAll bool, path string) (string, string, error) {
	m, err := locateAnchor(text, old)
	if err != nil {
		return text, "", err
	}
	if len(m.spans) >= maxAnchorSpans {
		return text, "", fmt.Errorf("old_string matches at least %d places in %s; nothing was changed — use a longer unique anchor", maxAnchorSpans, path)
	}
	if len(m.spans) > 1 && !replaceAll {
		return text, "", anchorAmbiguousError(path, text, m.spans, replaceAll, m.via)
	}
	// Replace from the end so earlier offsets stay valid.
	out := text
	for i := len(m.spans) - 1; i >= 0; i-- {
		sp := m.spans[i]
		out = out[:sp.start] + neu + out[sp.end:]
	}
	note := m.via
	if note != "" {
		note += fmt.Sprintf(", lines %d-%d", m.spans[0].firstLine, m.spans[0].lastLine)
		if len(m.spans) > 1 {
			note += fmt.Sprintf(" and %d more", len(m.spans)-1)
		}
	}
	return out, note, nil
}

// normalizeEOL converts a file's line endings to LF for editing and reports
// which ending to restore, so an edit on a CRLF file does not rewrite every
// line. Mixed-ending files are edited as-is (LF returned, no conversion).
func normalizeEOL(text string) (string, string) {
	if !strings.Contains(text, "\r\n") {
		return text, "\n"
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		return text, "\n"
	}
	return strings.ReplaceAll(text, "\r\n", "\n"), "\r\n"
}

// restoreEOL re-applies the file's original line endings.
func restoreEOL(text, eol string) string {
	if eol == "\n" {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", eol)
}

// alignToEOL rewrites an anchor or replacement that arrived with CRLF into the
// LF form the file is edited in, so a Windows-style anchor still matches
// byte-exactly instead of falling through to the loose pass.
func alignToEOL(s, eol string) string {
	if eol == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}
