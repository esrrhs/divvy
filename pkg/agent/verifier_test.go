package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

func TestVerifyExpectedOutputAcrossCommands(t *testing.T) {
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{log: SilentLogger()}

	node := &models.TaskNode{
		DoD: models.DoD{
			// First command emits nothing; only the second one contains
			// the expected text. Under the old per-command rule the quiet
			// first command made verification impossible.
			Commands:       []string{"true", "echo marker-ok"},
			ExpectedOutput: "marker-ok",
			TimeoutSec:     10,
		},
	}
	vr := o.verify(context.Background(), sb, node)
	if !vr.OK {
		t.Fatalf("expected pass from combined output, got: %s", vr.Output)
	}

	// Missing expected text after all commands passed → fail.
	node.DoD.ExpectedOutput = "nope"
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("expected failure for absent expected_output")
	}

	// A prose expected_output ("Build succeeds") on a quiet successful build
	// must be tolerated: exit codes are the verdict, not an unmatchable
	// sentence. This used to force the leaf into pointless re-splitting.
	node.DoD = models.DoD{
		Commands:       []string{"true"},
		ExpectedOutput: "Build succeeds",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); !vr.OK {
		t.Fatalf("prose expected_output on quiet success should pass: %s", vr.Output)
	}

	// A concrete token expected by a command that prints nothing still fails.
	node.DoD = models.DoD{
		Commands:       []string{"true"},
		ExpectedOutput: "ok",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("literal expected_output that is absent must fail")
	}

	// Prose expected_output is enforced when the commands actually printed
	// something (real mismatch must not be silently ignored).
	node.DoD = models.DoD{
		Commands:       []string{"echo real-output"},
		ExpectedOutput: "build returns success",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("mismatch with non-empty output should fail")
	}

	// Failing command still fails regardless of expected text.
	node.DoD = models.DoD{
		Commands:       []string{"echo marker-ok", "false"},
		ExpectedOutput: "marker-ok",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("expected failure from non-zero command")
	}
}

// TestVerify_DoDAbsoluteWorkspacePathRetargeted is the regression test for
// isolated leaves whose DoD commands hard-code the shared workspace path.
//
// The decomposer only ever sees the shared workspace, so a model routinely
// emits "cd /workspace && go test ./...". A leaf then runs inside an isolated
// mirror whose sandbox cwd is correct, but the explicit cd overrides it and
// verification silently inspects the *unmodified* workspace: Go reports
// "matched no packages", the DoD can never pass, the mirror never merges, and
// every retry burns budget on a failure unrelated to the code.
func TestVerify_DoDAbsoluteWorkspacePathRetargeted(t *testing.T) {
	ws, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Same file name, different content: whichever tree the command lands in
	// is unambiguous from the output.
	if err := ws.WriteFile("marker.txt", "workspace"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.WriteFile("marker.txt", "leaf"); err != nil {
		t.Fatal(err)
	}
	// A subdirectory proves subpaths survive retargeting (a leaf whose DoD
	// cds into a package dir must land in the mirror's copy of it).
	if err := leaf.WriteFile("sub/inner.txt", "leaf-sub"); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{log: SilentLogger(), sandbox: ws}

	// Isolated leaf: the baked-in workspace path must be followed into the
	// mirror, so the command sees the leaf's files.
	node := &models.TaskNode{DoD: models.DoD{
		Commands:       []string{"cd " + ws.Root + " && cat marker.txt"},
		ExpectedOutput: "leaf",
		TimeoutSec:     10,
	}}
	if vr := o.verify(context.Background(), leaf, node); !vr.OK {
		t.Fatalf("isolated leaf must verify in its own sandbox, got:\n%s", vr.Output)
	}

	// Subpath form: "cd <workspace>/sub" has to reach the mirror's sub.
	node.DoD = models.DoD{
		Commands:       []string{"cd " + ws.Root + "/sub && cat inner.txt"},
		ExpectedOutput: "leaf-sub",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), leaf, node); !vr.OK {
		t.Fatalf("subpath cd must retarget into the mirror, got:\n%s", vr.Output)
	}

	// Non-isolated run: sandbox IS the workspace, so behaviour is unchanged
	// (the command keeps pointing at the shared workspace).
	node.DoD = models.DoD{
		Commands:       []string{"cd " + ws.Root + " && cat marker.txt"},
		ExpectedOutput: "workspace",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), ws, node); !vr.OK {
		t.Fatalf("non-isolated verify must still run in the workspace, got:\n%s", vr.Output)
	}

	// No orchestrator sandbox (unit-callers construct a bare Orchestrator):
	// must not panic and must leave the command untouched.
	bare := &Orchestrator{log: SilentLogger()}
	node.DoD = models.DoD{
		Commands:       []string{"cd " + ws.Root + " && cat marker.txt"},
		ExpectedOutput: "workspace",
		TimeoutSec:     10,
	}
	if vr := bare.verify(context.Background(), ws, node); !vr.OK {
		t.Fatalf("bare orchestrator must not rewrite commands, got:\n%s", vr.Output)
	}
}

func TestDefaultDoDCommands_ByProject(t *testing.T) {
	cases := []struct {
		proj    tools.ProjectType
		wantSub string
		outputs []string
	}{
		{tools.ProjectGo, "go test ./...", nil},
		{tools.ProjectNode, "npm test --if-present", nil},
		{tools.ProjectRust, "cargo test", nil},
		{tools.ProjectPython, "py_compile", nil},
		{tools.ProjectGeneric, "ls", nil},
	}
	for _, c := range cases {
		cmds := defaultDoDCommands(c.outputs, c.proj)
		found := false
		for _, cmd := range cmds {
			if strings.Contains(cmd, c.wantSub) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected a command containing %q, got %v", c.proj, c.wantSub, cmds)
		}
	}

	// Declared outputs are always checked for existence first, in any
	// toolchain; generic projects never fall back to ls when outputs exist.
	cmds := defaultDoDCommands([]string{"app.py"}, tools.ProjectPython)
	if len(cmds) < 2 || !strings.HasPrefix(cmds[0], "test -f") {
		t.Fatalf("expected output existence check first, got %v", cmds)
	}
	cmds = defaultDoDCommands([]string{"notes.md"}, tools.ProjectGeneric)
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "test -f") {
		t.Fatalf("generic with outputs should only check existence, got %v", cmds)
	}
}

func TestEnsureGoalDoD_ByProject(t *testing.T) {
	want := map[tools.ProjectType]string{
		tools.ProjectGo:      "go build",
		tools.ProjectNode:    "npm test --if-present",
		tools.ProjectRust:    "cargo build",
		tools.ProjectPython:  "py_compile",
		tools.ProjectGeneric: "ls",
	}
	for proj, sub := range want {
		var dod models.DoD
		ensureGoalDoD(&dod, proj)
		if len(dod.Commands) == 0 {
			t.Errorf("%s: expected goal commands, got none", proj)
			continue
		}
		found := false
		for _, c := range dod.Commands {
			if strings.Contains(c, sub) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected a command with %q, got %v", proj, sub, dod.Commands)
		}
	}

	// Commands the model explicitly provided are never overwritten.
	dod := models.DoD{Commands: []string{"python3 app.py --selfcheck"}}
	ensureGoalDoD(&dod, tools.ProjectPython)
	if len(dod.Commands) != 1 || dod.Commands[0] != "python3 app.py --selfcheck" {
		t.Fatalf("model commands must take precedence, got %v", dod.Commands)
	}
}

func TestVerify_PythonProjectEndToEnd(t *testing.T) {
	// A Python workspace (pyproject marker) and a leaf whose DoD is empty:
	// verify() must detect Python, fill a py_compile syntax check, and
	// actually run it. Valid code passes; broken syntax fails.
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pyproject.toml", "[project]\nname='x'\n"); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{log: SilentLogger()}
	node := &models.TaskNode{
		Contract: models.ContractSpec{Outputs: []string{"app.py"}},
	}

	if err := sb.WriteFile("app.py", "def hello():\n    return 'ok'\n"); err != nil {
		t.Fatal(err)
	}
	vr := o.verify(context.Background(), sb, node)
	if !vr.OK {
		t.Fatalf("valid Python should pass the syntax check: %s", vr.Output)
	}

	// Now break the syntax: the generated default command must catch it.
	if err := sb.WriteFile("app.py", "def broken(:\n"); err != nil {
		t.Fatal(err)
	}
	node.DoD = models.DoD{} // force defaults to be regenerated
	vr = o.verify(context.Background(), sb, node)
	if vr.OK {
		t.Fatal("syntax-broken Python must fail verification")
	}
}

func TestVerify_GoFailurePrependsCompactDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("go.mod", "module broken\n\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("x.go", "package main\n\nfunc main() {\n\tundefinedSym = 1\n}\n"); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{log: SilentLogger()}
	node := &models.TaskNode{
		DoD: models.DoD{Commands: []string{"go build ./..."}, TimeoutSec: 60},
	}

	vr := o.verify(context.Background(), sb, node)
	if vr.OK {
		t.Fatal("broken Go code must fail verification")
	}
	// The compact block must LEAD the output: retried workers receive
	// prevError head-truncated, so root causes outside the head are lost.
	if !strings.HasPrefix(vr.Output, "[compact diagnostics:") {
		t.Fatalf("compact diagnostics must be prepended, got:\n%s", vr.Output)
	}
	if !strings.Contains(vr.Output, "x.go:") || !strings.Contains(vr.Output, "undefined: undefinedSym") {
		t.Fatalf("compiler error missing from diagnostics:\n%s", vr.Output)
	}
	// The raw log is still present after the separator.
	if !strings.Contains(vr.Output, "\n---\n$ go build") {
		t.Fatalf("raw output must follow the compact block:\n%s", vr.Output)
	}
}

func TestVerify_GrepFixedStringFallback(t *testing.T) {
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("test.go", "func TestAdd(t *testing.T) {\n\t// code\n}\n"); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{log: SilentLogger()}
	node := &models.TaskNode{
		DoD: models.DoD{
			Commands:   []string{`grep "(t *testing.T)" test.go`},
			TimeoutSec: 10,
		},
	}
	vr := o.verify(context.Background(), sb, node)
	if !vr.OK {
		t.Fatalf("expected grep fallback with -F to succeed, got: %s", vr.Output)
	}
}
