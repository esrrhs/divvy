package tools

import (
	"strconv"
	"strings"
	"testing"
)

func TestCompactDiagnostics_GoBuildErrors(t *testing.T) {
	raw := `# broken
./x.go:4:2: undefined: nope
/abs/path/y.go:9:13: invalid operation: a == b (mismatched types int and string)
FAIL	broken [build failed]
`
	got := CompactDiagnostics(ProjectGo, raw)
	if got == "" {
		t.Fatal("expected a compact block")
	}
	if !strings.HasPrefix(got, "[compact diagnostics:") {
		t.Fatalf("block must lead with the header:\n%s", got)
	}
	for _, want := range []string{
		"./x.go:4:2: undefined: nope",
		"/abs/path/y.go:9:13: invalid operation:",
		"FAIL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The block announces how many lines follow and yields back to raw output.
	if !strings.Contains(got, "3 compiler/test error line(s)") || !strings.Contains(got, "\n---") {
		t.Fatalf("header/separator wrong:\n%s", got)
	}
}

func TestCompactDiagnostics_GoTestFailures(t *testing.T) {
	raw := `--- FAIL: TestAdd (0.00s)
    math_test.go:12: got 3 want 4
panic: runtime error: index out of range [2] with length 2 [recovered]
FAIL	github.com/esrrhs/divvy/pkg/math	0.012s
FAIL
`
	got := CompactDiagnostics(ProjectGo, raw)
	for _, want := range []string{
		"--- FAIL: TestAdd",
		"math_test.go:12: got 3 want 4",
		"panic: runtime error",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The bare trailing FAIL and the tab-separated FAIL line are distinct
	// status lines but the framing must not fabricate duplicates.
	if strings.Count(got, "math_test.go:12") != 1 {
		t.Fatalf("diagnostic duplicated:\n%s", got)
	}
}

func TestCompactDiagnostics_DedupAndCap(t *testing.T) {
	// Same error reached via different indentation/tabs collapses to one.
	raw := "\ta.go:1:1: bad\n    a.go:1:1: bad\n"
	got := CompactDiagnostics(ProjectGo, raw)
	if strings.Count(got, "a.go:1:1: bad") != 1 {
		t.Fatalf("expected dedupe:\n%s", got)
	}

	// Beyond the cap: lines are omitted with an explicit note.
	var sb strings.Builder
	for i := 0; i < maxCompactDiagnostics+5; i++ {
		sb.WriteString("x.go:1:1: error number ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("\n")
	}
	got = CompactDiagnostics(ProjectGo, sb.String())
	if !strings.Contains(got, "5 more diagnostic line(s) omitted") {
		t.Fatalf("expected omission note:\n%s", got)
	}
	if strings.Contains(got, "error number 29") || strings.Contains(got, "error number 25") {
		t.Fatalf("cap not enforced:\n%s", got)
	}
}

func TestCompactDiagnostics_FiltersNoise(t *testing.T) {
	// Verifier framing and benign program output are not diagnostics.
	raw := "$ go test ./...\nexit 1\nok  	github.com/x/pkg	0.001s\n?  	github.com/x/empty	[no test files]\n"
	if got := CompactDiagnostics(ProjectGo, raw); got != "" {
		t.Fatalf("expected empty block for noise-only output, got:\n%s", got)
	}

	// Non-Go toolchains are left untouched.
	if got := CompactDiagnostics(ProjectPython, "./x.go:1:1: bad\n"); got != "" {
		t.Fatalf("python project must not get Go diagnostics: %s", got)
	}
	if got := CompactDiagnostics(ProjectGo, "   "); got != "" {
		t.Fatalf("blank output must produce empty block: %q", got)
	}
}

func TestCompactDiagnostics_LongLineClipped(t *testing.T) {
	long := strings.Repeat("x", maxDiagnosticLineLen+200)
	got := CompactDiagnostics(ProjectGo, "x.go:1:1: "+long+"\n")
	if !strings.Contains(got, "...[truncated]") {
		t.Fatalf("long diagnostic should be clipped:\n%s", got)
	}
}
