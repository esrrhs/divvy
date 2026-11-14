package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

// goSource is the fixture every anchor test edits: tabs for indentation, which
// is what gofmt writes and therefore what a model has to reproduce.
const goSource = `package calc

// Average returns the arithmetic mean of xs.
func Average(xs []int) float64 {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(len(xs)+1)
}
`

func newAnchorSandbox(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestReplaceText_IndentationDrift covers the most common weak-model slip:
// the anchor re-indented with spaces while the file uses tabs.
func TestReplaceText_IndentationDrift(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	anchor := "func Average(xs []int) float64 {\n" +
		"    sum := 0\n" +
		"    for _, x := range xs {\n" +
		"        sum += x\n" +
		"    }\n" +
		"    return float64(sum) / float64(len(xs)+1)\n" +
		"}"
	replacement := "func Average(xs []int) float64 {\n" +
		"\tif len(xs) == 0 {\n" +
		"\t\treturn 0\n" +
		"\t}\n" +
		"\tsum := 0\n" +
		"\tfor _, x := range xs {\n" +
		"\t\tsum += x\n" +
		"\t}\n" +
		"\treturn float64(sum) / float64(len(xs))\n" +
		"}"

	note, err := sb.ReplaceTextDetail("calc.go", anchor, replacement, false)
	if err != nil {
		t.Fatalf("indentation-tolerant match failed: %v", err)
	}
	if !strings.Contains(note, "whitespace") {
		t.Fatalf("note should explain the loose match, got %q", note)
	}
	got, err := sb.ReadFile("calc.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "if len(xs) == 0 {") {
		t.Fatalf("replacement missing:\n%s", got)
	}
	// The tabs of the untouched parts must survive: the engine replaces the
	// real region, not the model's re-indented copy.
	if !strings.Contains(got, "\tsum += x") {
		t.Fatalf("original indentation lost:\n%s", got)
	}
}

// TestReplaceText_ReadFileLineNumbers covers the other copy-paste failure: the
// model pastes read_file's "  4: " gutter into the anchor.
func TestReplaceText_ReadFileLineNumbers(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	anchor := "  5: \tsum := 0\n  6: \tfor _, x := range xs {\n  7: \t\tsum += x\n  8: \t}"
	replacement := "\tif len(xs) == 0 {\n\t\treturn 0\n\t}\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}"

	note, err := sb.ReplaceTextDetail("calc.go", anchor, replacement, false)
	if err != nil {
		t.Fatalf("line-numbered anchor should match: %v", err)
	}
	if !strings.Contains(note, "line numbers") {
		t.Fatalf("note should mention the dropped gutter, got %q", note)
	}
	got, _ := sb.ReadFile("calc.go")
	if !strings.Contains(got, "total += x") || strings.Contains(got, "sum += x") {
		t.Fatalf("wrong region replaced:\n%s", got)
	}
}

// TestReplaceText_TrailingWhitespace: editors strip trailing spaces, models
// often keep them; a diff on invisible characters must not dead-end the loop.
func TestReplaceText_TrailingWhitespace(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.txt", "alpha   \nbeta\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("t.txt", "alpha\nbeta\n", "one\ntwo\n", false); err != nil {
		t.Fatalf("trailing whitespace should be tolerated: %v", err)
	}
	got, _ := sb.ReadFile("t.txt")
	if got != "one\ntwo\n" {
		t.Fatalf("result = %q", got)
	}
}

// TestReplaceText_PartialLineFragment: an anchor that starts or ends mid-line
// still has to land on the right bytes.
func TestReplaceText_PartialLineFragment(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	// Space-indented fragment of one line's tail.
	if _, err := sb.ReplaceTextDetail("calc.go",
		"return float64(sum) / float64(len(xs)+1)", "return mean(sum, len(xs))", false); err != nil {
		t.Fatalf("fragment should match: %v", err)
	}
	got, _ := sb.ReadFile("calc.go")
	if !strings.Contains(got, "\treturn mean(sum, len(xs))\n}") {
		t.Fatalf("fragment replaced at the wrong place:\n%s", got)
	}
}

// TestReplaceText_ExactMatchKeepsNoteEmpty: the ordinary path must stay
// byte-exact and silent so "loose" never becomes the normal mode.
func TestReplaceText_ExactMatchKeepsNoteEmpty(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.txt", "alpha\nbeta\n"); err != nil {
		t.Fatal(err)
	}
	note, err := sb.ReplaceTextDetail("t.txt", "alpha", "ALPHA", false)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("exact match should produce no note, got %q", note)
	}
}

// TestReplaceText_LooseAmbiguityStillErrors: tolerance must never turn into a
// guess. Two whitespace-different twins are ambiguous, and the error has to say
// where they are.
func TestReplaceText_LooseAmbiguityStillErrors(t *testing.T) {
	sb := newAnchorSandbox(t)
	src := "func a() {\n\tprint(1)\n}\nfunc b() {\n    print(1)\n}\n"
	if err := sb.WriteFile("t.go", src); err != nil {
		t.Fatal(err)
	}
	_, err := sb.ReplaceTextDetail("t.go", "        print(1)\n}", "print(2)\n}", false)
	if err == nil {
		t.Fatal("two loose matches without replace_all must error")
	}
	msg := err.Error()
	for _, want := range []string{"occurs 2 times", "lines 2-3", "lines 5-6"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if got, _ := sb.ReadFile("t.go"); got != src {
		t.Fatalf("failed edit changed the file: %q", got)
	}
}

// TestReplaceText_LooseReplaceAll: with replace_all the same anchor replaces
// every hit, and the file's own indentation survives because only the matched
// region is swapped.
func TestReplaceText_LooseReplaceAll(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.go", "func a() {\n\tprint(1)\n}\nfunc b() {\n        print(1)\n}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("t.go", "\t\tprint(1)\n}", "print(2)\n}", true); err != nil {
		t.Fatal(err)
	}
	got, _ := sb.ReadFile("t.go")
	want := "func a() {\n\tprint(2)\n}\nfunc b() {\n        print(2)\n}\n"
	if got != want {
		t.Fatalf("replace_all = %q, want %q", got, want)
	}
}

// TestReplaceText_NotFoundQuotesClosestRegion: the failure message is the
// model's only feedback, so it has to carry real line numbers and real text.
func TestReplaceText_NotFoundQuotesClosestRegion(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	_, err := sb.ReplaceTextDetail("calc.go",
		"sum := 0\nfor _, x := range xs {\n  sum += x * 2\n}", "x", false)
	if err == nil {
		t.Fatal("missing anchor must error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not found") {
		t.Fatalf("error should say not found: %q", msg)
	}
	if !strings.Contains(msg, "closest region, lines 5-") {
		t.Fatalf("error should quote the closest region: %q", msg)
	}
	if !strings.Contains(msg, "sum += x") {
		t.Fatalf("error should quote the real file text: %q", msg)
	}
	if !strings.Contains(msg, "old_string") {
		t.Fatalf("error should tell the model what to fix: %q", msg)
	}
}

// TestClosestRegionHintSilentOnNonsense: quoting an unrelated region would be
// worse than saying nothing.
func TestClosestRegionHintSilentOnNonsense(t *testing.T) {
	if hint := closestRegionHint(goSource, "zzzz qqqq wwww\nvvvv uuuu"); hint != "" {
		t.Fatalf("unrelated anchor should produce no hint, got %q", hint)
	}
	if hint := closestRegionHint("", "x"); hint != "" {
		t.Fatalf("empty file should produce no hint, got %q", hint)
	}
}

// TestReplaceTexts_LooseAnchorStaysAtomic: a tolerated first anchor must not
// let a failing second anchor half-apply.
func TestReplaceTexts_LooseAnchorStaysAtomic(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	err := sb.ReplaceTexts("calc.go", []TextEdit{
		{Old: "    sum := 0", New: "\ttotal := 0"},
		{Old: "no-such-line", New: "x"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "edits[1]") {
		t.Fatalf("expected a failing second edit, got %v", err)
	}
	got, _ := sb.ReadFile("calc.go")
	if got != goSource {
		t.Fatalf("failed batch touched the file:\n%s", got)
	}
}

// TestReplaceTexts_NotesLooseEdits: the batch note names which edit needed
// help, so a model can see its own pattern.
func TestReplaceTexts_NotesLooseEdits(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.txt", "alpha\nbeta\n"); err != nil {
		t.Fatal(err)
	}
	note, err := sb.ReplaceTextsDetail("t.txt", []TextEdit{
		{Old: "alpha", New: "ALPHA"},
		{Old: "  beta", New: "BETA"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "edits[1]") {
		t.Fatalf("note should name the loose edit, got %q", note)
	}
	if strings.Contains(note, "edits[0]") {
		t.Fatalf("exact edit should not be reported as loose: %q", note)
	}
	got, _ := sb.ReadFile("t.txt")
	if got != "ALPHA\nBETA\n" {
		t.Fatalf("result = %q", got)
	}
}

// TestReplaceText_PreservesCRLF: editing a Windows-checked-out file must not
// rewrite every line ending.
func TestReplaceText_PreservesCRLF(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("win.go", "package a\r\n\r\nfunc f() {}\r\n"); err != nil {
		t.Fatal(err)
	}
	note, err := sb.ReplaceTextDetail("win.go", "func f() {}\r\n", "func f() { println(1) }\r\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("CRLF anchors should match exactly after alignment, note=%q", note)
	}
	raw := readFile(t, filepath.Join(sb.Root, "win.go"))
	if raw != "package a\r\n\r\nfunc f() { println(1) }\r\n" {
		t.Fatalf("line endings changed: %q", raw)
	}
}

// TestReplaceLines_Unaffected keeps the line-range mode honest: this change is
// about anchors, not ranges.
func TestReplaceLines_Unaffected(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.txt", "a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceLines("t.txt", 2, 2, "B"); err != nil {
		t.Fatal(err)
	}
	got, _ := sb.ReadFile("t.txt")
	if got != "a\nB\nc\n" {
		t.Fatalf("result = %q", got)
	}
}

// TestStripAnchorLineNumbers isolates the gutter detector: it must fire only
// when every content line is numbered.
func TestStripAnchorLineNumbers(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		stored bool
	}{
		{"  1: a\n  2: b", "a\nb", true},
		{"1: a", "a", true},
		{"a\nb", "a\nb", false},
		{"1: a\nb", "1: a\nb", false},
		{"  1: a\n\n  2: b", "a\n\nb", true},
		{"", "", false},
	} {
		got, ok := stripAnchorLineNumbers(tc.in)
		if ok != tc.stored || got != tc.want {
			t.Fatalf("strip(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.stored)
		}
	}
}

// TestTrimBlankEdges keeps the padding trim from eating interior blank lines,
// and from eating the single newline that terminates the last content line.
func TestTrimBlankEdges(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\n\na\n\nb\n\n", "a\n\nb\n"},
		{"\n\n", ""},
		{"a", "a"},
		{"a\n", "a\n"},
		{"\n\na\n", "a\n"},
	} {
		if got := trimBlankEdges(tc.in); got != tc.want {
			t.Fatalf("trimBlankEdges(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestReplaceText_ReplaceAllManyOccurrences guards the span cap: replace_all on
// a common anchor must replace every occurrence, not just the first page of
// matches it enumerated.
func TestReplaceText_ReplaceAllManyOccurrences(t *testing.T) {
	sb := newAnchorSandbox(t)
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("x\n")
	}
	if err := sb.WriteFile("many.txt", b.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("many.txt", "x", "y", true); err != nil {
		t.Fatal(err)
	}
	got, _ := sb.ReadFile("many.txt")
	if strings.Contains(got, "x") || strings.Count(got, "y") != 500 {
		t.Fatalf("replace_all missed occurrences: %d replaced", strings.Count(got, "y"))
	}
}

// TestReplaceText_BlankAnchorRejected keeps a whitespace-only anchor from
// turning into a match-everything search.
func TestReplaceText_BlankAnchorRejected(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("t.txt", "a\n\nb\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReplaceTextDetail("t.txt", "   \n\t\n", "x", false); err == nil {
		t.Fatal("blank anchor must be rejected")
	}
	if got, _ := sb.ReadFile("t.txt"); got != "a\n\nb\n" {
		t.Fatalf("rejected edit changed the file: %q", got)
	}
}
