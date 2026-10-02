package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/models"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
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
