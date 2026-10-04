package tools

import (
	"strconv"
	"strings"
	"testing"
)

// TestDiagRuleForCoversEveryProjectType is the guard that motivated the
// Make/Generic rules: a toolchain with no rule gets an unfiltered build log,
// which is exactly the context bloat the compact block exists to prevent.
func TestDiagRuleForCoversEveryProjectType(t *testing.T) {
	all := []ProjectType{
		ProjectGo, ProjectNode, ProjectRust, ProjectPython, ProjectMake, ProjectGeneric,
	}
	for _, pt := range all {
		if _, ok := diagRuleFor(pt); !ok {
			t.Errorf("no diagnostic rule for %q; its failures would be passed through unfiltered", pt)
		}
	}
	if _, ok := diagRuleFor("cobol"); ok {
		t.Error("an unknown project type should have no rule")
	}
}

func TestSupportedDiagnosticProjects(t *testing.T) {
	got := SupportedDiagnosticProjects()
	if len(got) != len(diagRules) {
		t.Fatalf("len = %d, want %d", len(got), len(diagRules))
	}
	seen := map[ProjectType]bool{}
	for _, pt := range got {
		if seen[pt] {
			t.Errorf("duplicate entry for %q", pt)
		}
		seen[pt] = true
	}
	for _, want := range []ProjectType{ProjectGo, ProjectNode, ProjectRust, ProjectPython, ProjectMake, ProjectGeneric} {
		if !seen[want] {
			t.Errorf("SupportedDiagnosticProjects is missing %q", want)
		}
	}
}

func TestCompactDiagnostics_MakeTargetFailure(t *testing.T) {
	raw := `cc -O2 -c src/a.c -o build/a.o
make: *** [Makefile:12: build] Error 1
make: *** [Makefile:20: test] Error 2
`
	got := CompactDiagnostics(ProjectMake, raw)
	if got == "" {
		t.Fatal("a failing make run must produce a compact block")
	}
	if !strings.Contains(got, "Makefile:12: build") {
		t.Errorf("missing the failing target line:\n%s", got)
	}
	// The compile command line is noise; it must not crowd out the verdict.
	if strings.Contains(got, "cc -O2 -c") {
		t.Errorf("compilation command should be filtered out:\n%s", got)
	}
}

func TestCompactDiagnostics_MakeSurfacesCompilerErrors(t *testing.T) {
	raw := `gcc -c a.c
src/a.c:5:3: error: expected ';' before '}' token
src/a.c:5:3: note: to match this '{'
`
	got := CompactDiagnostics(ProjectMake, raw)
	if !strings.Contains(got, "expected ';'") {
		t.Errorf("a compiler error surfaced through make should be kept:\n%s", got)
	}
}

func TestCompactDiagnostics_GenericAnchorsPathLineErrors(t *testing.T) {
	raw := `starting build
src/a.c:5:3: error: undeclared identifier
linking...
some completely unrelated chatter
`
	got := CompactDiagnostics(ProjectGeneric, raw)
	if got == "" {
		t.Fatal("an unmarked workspace should still get a compact block")
	}
	if !strings.Contains(got, "undeclared identifier") {
		t.Errorf("missing the anchored error:\n%s", got)
	}
	if strings.Contains(got, "some completely unrelated chatter") {
		t.Errorf("unrelated chatter must be filtered:\n%s", got)
	}
}

func TestCompactDiagnostics_GenericCatchesCommonVerdicts(t *testing.T) {
	raw := `running tests
--- FAIL: TestThing (0.00s)
ValueError: bad input
fatal error: cannot continue
`
	got := CompactDiagnostics(ProjectGeneric, raw)
	for _, want := range []string{"--- FAIL: TestThing", "ValueError: bad input", "fatal error: cannot continue"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestCompactDiagnostics_GenericIgnoresPureNoise(t *testing.T) {
	// Nothing here names a failure, so no block is better than a misleading one.
	raw := "downloading\ndone in 3s\nhello world\n"
	if got := CompactDiagnostics(ProjectGeneric, raw); got != "" {
		t.Errorf("expected no block for pure noise, got:\n%s", got)
	}
}

func TestCompactDiagnostics_PythonPyCompileErrors(t *testing.T) {
	raw := `  File "/usr/lib/python3.11/py_compile.py", line 1
SyntaxError: invalid syntax
app.py:12: unexpected indent
`
	got := CompactDiagnostics(ProjectPython, raw)
	if !strings.Contains(got, "SyntaxError: invalid syntax") {
		t.Errorf("missing the exception headline:\n%s", got)
	}
	if !strings.Contains(got, "app.py:12") {
		t.Errorf("missing the py_compile file:line form:\n%s", got)
	}
}

func TestCompactDiagnostics_EmptyInput(t *testing.T) {
	for _, pt := range []ProjectType{ProjectGo, ProjectMake, ProjectGeneric} {
		if got := CompactDiagnostics(pt, ""); got != "" {
			t.Errorf("%s: empty output should produce no block, got %q", pt, got)
		}
		if got := CompactDiagnostics(pt, "   \n  \n"); got != "" {
			t.Errorf("%s: blank output should produce no block, got %q", pt, got)
		}
	}
}

func TestCompactDiagnostics_DedupesAndOrdersRootCausesFirst(t *testing.T) {
	raw := `noise before
./x.go:1:1: first problem
some chatter
./x.go:1:1: first problem
./x.go:2:2: second problem
`
	got := CompactDiagnostics(ProjectGo, raw)
	if strings.Count(got, "first problem") != 1 {
		t.Errorf("the same error reached twice should appear once:\n%s", got)
	}
	firstAt := strings.Index(got, "first problem")
	secondAt := strings.Index(got, "second problem")
	if firstAt < 0 || secondAt < 0 || firstAt > secondAt {
		t.Errorf("root causes must keep source order:\n%s", got)
	}
	// The header must come before any diagnostic.
	if !strings.HasPrefix(got, "[compact diagnostics:") {
		t.Errorf("block must lead with the header:\n%s", got)
	}
}

func TestCompactDiagnostics_CapsLineCount(t *testing.T) {
	// Each line must be distinct, otherwise dedup (which runs first) would
	// legitimately collapse them and the cap would never be reached.
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("./x.go:")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(":1: error number\n")
	}
	got := CompactDiagnostics(ProjectGo, b.String())
	// maxCompactDiagnostics lines plus a header, a truncation note and a
	// separator; anything more would crowd the prompt.
	if n := strings.Count(got, "error number"); n != maxCompactDiagnostics {
		t.Errorf("kept %d diagnostics, want the cap of %d", n, maxCompactDiagnostics)
	}
	if !strings.Contains(got, "omitted") {
		t.Errorf("a capped block must say how many were dropped:\n%s", got)
	}
	if !strings.Contains(got, "more diagnostic line(s) omitted") {
		t.Errorf("missing the omission note:\n%s", got)
	}
	// The header count must reflect every match, not just the kept ones.
	if !strings.Contains(got, "60 compiler/test error line(s)") {
		t.Errorf("header should report the true total:\n%s", got)
	}
}

func TestCompactDiagnostics_ClipsVeryLongLines(t *testing.T) {
	long := "./x.go:1:1: " + strings.Repeat("e", 1000)
	got := CompactDiagnostics(ProjectGo, long)
	if !strings.Contains(got, "truncated") {
		t.Errorf("an over-long diagnostic should be clipped:\n%s", got)
	}
	// The clip marker itself must not push the line past a sane prompt size.
	for _, line := range strings.Split(got, "\n") {
		if len(line) > maxDiagnosticLineLen+32 {
			t.Errorf("clipped line is still %d bytes:\n%s", len(line), line)
		}
	}
}

func TestClipDiagnosticLine(t *testing.T) {
	if got := clipDiagnosticLine("  x  "); got != "  x" {
		t.Errorf("trailing whitespace should be trimmed, got %q", got)
	}
	short := "short"
	if got := clipDiagnosticLine(short); got != short {
		t.Errorf("short line altered: %q", got)
	}
	long := strings.Repeat("x", maxDiagnosticLineLen+50)
	got := clipDiagnosticLine(long)
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("long line should end with the clip marker, got %q", got[len(got)-20:])
	}
}

// TestDiagRulesAreAllDistinct guards against two entries claiming the same
// project type, where the first would silently win.
func TestDiagRulesAreAllDistinct(t *testing.T) {
	seen := map[ProjectType]bool{}
	for _, r := range diagRules {
		if seen[r.project] {
			t.Errorf("duplicate rule for %q", r.project)
		}
		seen[r.project] = true
	}
}
