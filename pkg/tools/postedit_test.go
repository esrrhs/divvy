package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlanEditCheck_Go(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("pkg/a.go")
	mk("pkg/a_test.go")
	mk("root.go")
	mk("vendor/v/x.go")

	project, cmds, skip := planEditCheck(root, []string{"pkg/a.go"})
	if project != ProjectGo || skip != "" {
		t.Fatalf("project=%v skip=%q", project, skip)
	}
	if len(cmds) != 1 || cmds[0] != "go build ./'pkg'" {
		t.Fatalf("cmds=%q", cmds)
	}

	// Test files compile only under `go test`, so vet is the cheap check.
	_, cmds, _ = planEditCheck(root, []string{"pkg/a_test.go"})
	if len(cmds) != 1 || cmds[0] != "go vet ./'pkg'" {
		t.Fatalf("test file cmds=%q", cmds)
	}

	_, cmds, _ = planEditCheck(root, []string{"root.go"})
	if len(cmds) != 1 || cmds[0] != "go build ." {
		t.Fatalf("root cmds=%q", cmds)
	}

	// vendor/ is not ours to verify, and a deleted file cannot be checked.
	_, cmds, skip = planEditCheck(root, []string{"vendor/v/x.go", "gone.go"})
	if len(cmds) != 0 || skip == "" {
		t.Fatalf("vendor/gone: cmds=%q skip=%q", cmds, skip)
	}
}

func TestPlanEditCheck_CapsCommands(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, d := range []string{"a", "b", "c", "d", "e"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "x.go")
		if err := os.WriteFile(filepath.Join(root, p), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	_, cmds, _ := planEditCheck(root, paths)
	if len(cmds) != maxEditCheckCommands {
		t.Fatalf("expected %d commands, got %q", maxEditCheckCommands, cmds)
	}
}

// The check follows the workspace's toolchain, not the edited file's
// extension: a stray .go file in a Rust repo is not silently compiled with a
// guessed module root.
func TestPlanEditCheck_FollowsWorkspaceToolchain(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, cmds, skip := planEditCheck(root, []string{"a.go"})
	if project != ProjectRust || len(cmds) != 0 || !strings.Contains(skip, "not Rust source") {
		t.Fatalf("project=%v cmds=%q skip=%q", project, cmds, skip)
	}
}

func TestPlanEditCheck_PythonAndNode(t *testing.T) {
	py := t.TempDir()
	if err := os.WriteFile(filepath.Join(py, "requirements.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(py, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, cmds, skip := planEditCheck(py, []string{"app.py"})
	if project != ProjectPython || len(cmds) != 1 || skip != "" {
		t.Fatalf("python: project=%v cmds=%q skip=%q", project, cmds, skip)
	}
	if !strings.Contains(cmds[0], "ast.parse") {
		t.Fatalf("python check should not write __pycache__: %q", cmds[0])
	}

	node := t.TempDir()
	if err := os.WriteFile(filepath.Join(node, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "index.js"), []byte("let x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cmds, _ := planEditCheck(node, []string{"index.js"}); len(cmds) != 1 || !strings.HasPrefix(cmds[0], "node --check ") {
		t.Fatalf("node cmds=%q", cmds)
	}
	// TypeScript has no cheap single-file check without project config.
	if err := os.WriteFile(filepath.Join(node, "a.ts"), []byte("export const x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cmds, skip := planEditCheck(node, []string{"a.ts"}); len(cmds) != 0 || skip == "" {
		t.Fatalf("ts: cmds=%q skip=%q", cmds, skip)
	}
}

func TestPlanEditCheck_Generic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cmds, skip := planEditCheck(root, []string{"main.c"}); len(cmds) != 0 || skip == "" {
		t.Fatalf("generic: cmds=%q skip=%q", cmds, skip)
	}
}

func TestEditCheckBlock(t *testing.T) {
	ok := EditCheck{Commands: []string{"go build ./pkg"}, Duration: 120 * time.Millisecond}
	if block := ok.Block(); !strings.Contains(block, "OK") || !strings.Contains(block, "go build ./pkg") {
		t.Fatalf("ok block=%q", block)
	}
	bad := EditCheck{Commands: []string{"go build ./pkg"}, Failed: true, Output: "pkg/a.go:3:1: syntax error"}
	block := bad.Block()
	if !strings.Contains(block, "FAILED") || !strings.Contains(block, "syntax error") || !strings.Contains(block, "Do not call finish") {
		t.Fatalf("failed block=%q", block)
	}
	skip := EditCheck{Skipped: "no cheap check"}
	if got := skip.Block(); !strings.Contains(got, "skipped") {
		t.Fatalf("skip block=%q", got)
	}
	if (EditCheck{}).Block() != "" {
		t.Fatal("empty check should render nothing")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"pkg/a.go":   "'pkg/a.go'",
		"a b/c.go":   "'a b/c.go'",
		"it's.go":    `'it'\''s.go'`,
		"; rm -rf /": "'; rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRunEditChecks_FailureStopsAndCompacts(t *testing.T) {
	var ran []string
	check := runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
		ran = append(ran, cmd)
		if cmd == "go build ./'pkg'" {
			return &ExecResult{
				Stderr:   "pkg/a.go:3:1: syntax error: unexpected newline\nnot a diagnostic line\n",
				ExitCode: 1,
			}, nil
		}
		return &ExecResult{ExitCode: 0}, nil
	}, ProjectGo, []string{"go build ./'pkg'", "go build ./'other'"}, time.Second)

	if !check.Failed || check.TimedOut {
		t.Fatalf("expected failure, got %+v", check)
	}
	if len(ran) != 1 {
		t.Fatalf("must stop at the first failure, ran=%q", ran)
	}
	if !strings.Contains(check.Output, "pkg/a.go:3:1") {
		t.Fatalf("output should keep the diagnostic: %q", check.Output)
	}
	if !strings.Contains(check.Block(), "FAILED") {
		t.Fatalf("block=%q", check.Block())
	}
}

func TestRunEditChecks_SuccessAndTimeout(t *testing.T) {
	ok := runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
		return &ExecResult{ExitCode: 0}, nil
	}, ProjectGo, []string{"a", "b"}, time.Second)
	if ok.Failed || !ok.Ran() || len(ok.Commands) != 2 {
		t.Fatalf("ok=%+v", ok)
	}

	slow := runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
		return &ExecResult{TimedOut: true, Stdout: "partial"}, nil
	}, ProjectRust, []string{"cargo check --quiet"}, 50*time.Millisecond)
	if !slow.Failed || !slow.TimedOut {
		t.Fatalf("slow=%+v", slow)
	}
	if !strings.Contains(slow.Block(), "TIMED OUT") {
		t.Fatalf("block=%q", slow.Block())
	}
}

func TestRunEditChecks_RunnerErrorIsSkipNotFailure(t *testing.T) {
	// A host without isolation cannot run the check. That is not the model's
	// fault and must not block finish.
	check := runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
		return nil, ErrExecutionUnavailable
	}, ProjectGo, []string{"go build ."}, time.Second)
	if check.Ran() || check.Failed {
		t.Fatalf("unavailable runner should be a skip: %+v", check)
	}
	if !strings.Contains(check.Skipped, "could not run") {
		t.Fatalf("skipped=%q", check.Skipped)
	}
}

func TestRenderEditCheckOutput_Caps(t *testing.T) {
	long := strings.Repeat("x", editCheckOutputCap*2)
	out := renderEditCheckOutput(ProjectGeneric, long)
	if len(out) > editCheckOutputCap+100 {
		t.Fatalf("output not capped: %d", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("cap should be disclosed: %q", out[len(out)-40:])
	}
}

// On a host without filesystem isolation the planned build commands cannot run.
// For Go files the check degrades to the in-process parser (labelled as a syntax
// parse, never as a build); for anything else it degrades to a skip and never to
// an unsandboxed run.
func TestVerifyEditedFiles_FallsBackToSyntaxWhenShellUnavailable(t *testing.T) {
	root := t.TempDir()
	if ok, _ := ProbeShellIsolation(root); ok {
		t.Skip("host supports isolated execution; this asserts the fallback path")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package m\n\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyRuntimePolicy(sb)
	check := sb.VerifyEditedFiles(context.Background(), []string{"a.go"}, time.Second)
	if !check.Ran() {
		t.Fatalf("expected the syntax fallback to run, got %+v", check)
	}
	if !check.SyntaxOnly() {
		t.Fatalf("fallback must be labelled as a parse, got %v", check.Commands)
	}
	if !check.Failed {
		t.Fatalf("broken Go source must fail the parse check: %+v", check)
	}
	if !strings.Contains(check.Output, "a.go:3") {
		t.Fatalf("diagnostics should point at the bad line: %q", check.Output)
	}

	// A non-Go edit has no in-process checker, so it must skip with a reason
	// rather than silently reporting success.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skip := sb.VerifyEditedFiles(context.Background(), []string{"b.txt"}, time.Second)
	if skip.Ran() || skip.Failed {
		t.Fatalf("expected a skip for a non-Go edit, got %+v", skip)
	}
	if skip.Skipped == "" {
		t.Fatalf("a skip must carry a reason, got %+v", skip)
	}
}

// TestGoSyntaxCommands_FiltersAndCaps locks what the no-shell fallback will and
// will not parse: Go files only, never vendor/testdata, never missing paths, and
// never more than the edit-check budget.
func TestGoSyntaxCommands_FiltersAndCaps(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a.go", "b.go", "c.go", "d.go", "vendor/v/x.go", "testdata/y.go", "notes.txt"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package m\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmds := goSyntaxCommands(root, []string{
		"a.go", "./b.go", "a.go", "vendor/v/x.go", "testdata/y.go", "notes.txt", "gone.go", "../escape.go", "c.go", "d.go",
	})
	if len(cmds) != maxEditCheckCommands {
		t.Fatalf("want %d capped commands, got %v", maxEditCheckCommands, cmds)
	}
	want := []string{builtinGoSyntaxPrefix + "a.go", builtinGoSyntaxPrefix + "b.go", builtinGoSyntaxPrefix + "c.go"}
	for i, w := range want {
		if cmds[i] != w {
			t.Fatalf("cmds = %v, want prefix %v", cmds, want)
		}
	}
}

// TestGoSyntaxTarget round-trips the label used in the recheck list.
func TestGoSyntaxTarget(t *testing.T) {
	if f, ok := goSyntaxTarget("go syntax pkg/a.go"); !ok || f != "pkg/a.go" {
		t.Fatalf("goSyntaxTarget = %q,%v", f, ok)
	}
	for _, cmd := range []string{"go build ./...", "go syntax ", "", "node --check a.js"} {
		if _, ok := goSyntaxTarget(cmd); ok {
			t.Fatalf("%q must not be treated as a builtin", cmd)
		}
	}
}

// TestCheckGoSyntax_ReportsPosition: the output has to look like a compiler's,
// otherwise the diagnosis compaction (which keys off file:line:col) is useless.
func TestCheckGoSyntax_ReportsPosition(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package m\n\nfunc F() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.go"), []byte("package m\n\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if res := sb.checkGoSyntax("ok.go"); res.ExitCode != 0 || res.Stderr != "" {
		t.Fatalf("valid file reported %+v", res)
	}
	res := sb.checkGoSyntax("bad.go")
	if res.ExitCode == 0 {
		t.Fatal("broken file must exit non-zero")
	}
	if !strings.Contains(res.Stderr, "bad.go:3:") {
		t.Fatalf("diagnostics should carry file:line:col, got %q", res.Stderr)
	}
	if res := sb.checkGoSyntax("missing.go"); res.ExitCode == 0 {
		t.Fatal("missing file must exit non-zero")
	}
}

// TestRunEditCheckCommands_DispatchesBuiltin is the point of the fallback: a
// parse check runs with the shell disabled, and a mixed list does not need the
// shell for its builtin entries.
func TestRunEditCheckCommands_DispatchesBuiltin(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.go"), []byte("package m\n\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	sb.DisableTool(ToolRunBash, "test: no shell")

	ok := sb.RunEditCheckCommands(context.Background(), ProjectGo, []string{builtinGoSyntaxPrefix + "bad.go"}, time.Second)
	if ok.Skipped != "" {
		t.Fatalf("builtin must not be skipped for lack of a shell: %+v", ok)
	}
	if !ok.Ran() || !ok.Failed || !ok.SyntaxOnly() {
		t.Fatalf("builtin check result = %+v", ok)
	}
	if !strings.Contains(ok.Block(), "FAILED") || !strings.Contains(ok.Block(), "bad.go:3") {
		t.Fatalf("block should carry the failure:\n%s", ok.Block())
	}
}

// TestEditCheck_SyntaxOnlyWording keeps the pass message honest: a parse is not
// a build, and the model must not read it as one.
func TestEditCheck_SyntaxOnlyWording(t *testing.T) {
	parse := EditCheck{Commands: []string{builtinGoSyntaxPrefix + "a.go"}}
	if !parse.SyntaxOnly() {
		t.Fatal("parse-only check should report SyntaxOnly")
	}
	if block := parse.Block(); !strings.Contains(block, "Syntax parses") || strings.Contains(block, "compile/type errors") {
		t.Fatalf("parse-only block must not claim a build: %q", block)
	}
	build := EditCheck{Commands: []string{"go build ."}}
	if build.SyntaxOnly() {
		t.Fatal("a shell build is not syntax-only")
	}
	if block := build.Block(); !strings.Contains(block, "No compile/type errors") {
		t.Fatalf("build block wording changed: %q", block)
	}
	mixed := EditCheck{Commands: []string{builtinGoSyntaxPrefix + "a.go", "go build ."}}
	if mixed.SyntaxOnly() {
		t.Fatal("a mixed list includes a real build")
	}
}
