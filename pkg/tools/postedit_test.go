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

// On a host without filesystem isolation the check must degrade to a skip, not
// to a failure (which would block every finish) and not to an unsandboxed run.
func TestVerifyEditedFiles_SkipsWhenShellUnavailable(t *testing.T) {
	root := t.TempDir()
	if ok, _ := ProbeShellIsolation(root); ok {
		t.Skip("host supports isolated execution; this asserts the fallback path")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyRuntimePolicy(sb)
	check := sb.VerifyEditedFiles(context.Background(), []string{"a.go"}, time.Second)
	if check.Ran() || check.Failed {
		t.Fatalf("expected a skip, got %+v", check)
	}
	if !strings.Contains(check.Skipped, "unavailable") {
		t.Fatalf("skip reason should say execution is unavailable: %q", check.Skipped)
	}
}
