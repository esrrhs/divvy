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

func TestCompactDiagnostics_Python(t *testing.T) {
	raw := `Traceback (most recent call last):
  File "/app/svc.py", line 12, in main
    result = fetch()
  File "app/util.py", line 42, in fetch
    return data["k"]
KeyError: 'k'
`
	got := CompactDiagnostics(ProjectPython, raw)
	for _, want := range []string{
		`File "/app/svc.py", line 12, in main`,
		`File "app/util.py", line 42, in fetch`,
		"KeyError: 'k'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("python diagnostics missing %q:\n%s", want, got)
		}
	}
	// Source code lines shown under the File frame are not diagnostics.
	if strings.Contains(got, "result = fetch()") {
		t.Fatalf("frame source line should be excluded:\n%s", got)
	}

	// pytest "E" exception lines and FAILED summaries are recognized.
	pytest := `FAILED tests/test_x.py::TestAdd - AssertionError: assert 1 == 2

=== short test summary info ===
` +
		"tests/test_x.py:3: AssertionError: assert 1 == 2\n" + `
E       AssertionError: assert 1 == 2
`
	got = CompactDiagnostics(ProjectPython, pytest)
	if !strings.Contains(got, "FAILED tests/test_x.py") ||
		!strings.Contains(got, "E       AssertionError") {
		t.Fatalf("pytest diagnostics missing:\n%s", got)
	}
}

func TestCompactDiagnostics_Rust(t *testing.T) {
	raw := `   Compiling demo v0.1.0
error[E0425]: cannot find value nope in this scope
 --> src/main.rs:3:9
  |
3 |     nope;
  |     ^^^^ not found in this scope

error: aborting due to 1 previous error
`
	got := CompactDiagnostics(ProjectRust, raw)
	for _, want := range []string{
		"error[E0425]: cannot find value",
		"--> src/main.rs:3:9",
		"error: aborting due to",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rust diagnostics missing %q:\n%s", want, got)
		}
	}
	// Source context lines are not diagnostics.
	if strings.Contains(got, "nope;") {
		t.Fatalf("source line should be excluded:\n%s", got)
	}
}

func TestCompactDiagnostics_NodeAndTSC(t *testing.T) {
	// tsc output.
	tsc := `src/x.ts(10,5): error TS2304: Cannot find name 'y'.
src/y.ts(2,1): error TS1005: ';' expected.
`
	got := CompactDiagnostics(ProjectNode, tsc)
	if !strings.Contains(got, "src/x.ts(10,5): error TS2304") ||
		!strings.Contains(got, "src/y.ts(2,1): error TS1005") {
		t.Fatalf("tsc diagnostics missing:\n%s", got)
	}

	// Node runtime error: location line + exception header.
	node := `/app/x.js:3
foo();
^

ReferenceError: foo is not defined
    at Object.<anonymous> (/app/x.js:3:1)
    at Module._compile (node:internal/modules/cjs/loader:1368:35)
`
	got = CompactDiagnostics(ProjectNode, node)
	if !strings.Contains(got, "/app/x.js:3") || !strings.Contains(got, "ReferenceError: foo is not defined") {
		t.Fatalf("node runtime diagnostics missing:\n%s", got)
	}
	// Indented stack frames are intentionally excluded (they are noisy; the
	// location line and exception header carry the root cause).
	if strings.Contains(got, "at Object.<anonymous>") {
		t.Fatalf("stack frames should be excluded:\n%s", got)
	}
}

func TestCompactDiagnostics_OtherStacksUnsupported(t *testing.T) {
	// Make/generic projects and the empty case produce nothing.
	if got := CompactDiagnostics(ProjectMake, "x.go:1:1: bad\n"); got != "" {
		t.Fatalf("make project must not use Go patterns: %q", got)
	}
	if got := CompactDiagnostics(ProjectGeneric, "whatever\n"); got != "" {
		t.Fatalf("generic project has no diagnostics matcher: %q", got)
	}
}
