package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// The per-edit check compiles the package that was edited. That is the right
// cheap check for one edit and the wrong gate for a turn: change a function's
// signature and every package that calls it stops compiling — and none of
// those packages is compiled by the edited package's own build. So a turn
// could finish "successfully" with a workspace that does not build.
//
// These tests use a REAL Go workspace on purpose: the failure is about which
// packages the compiler looks at, and an injected check would prove nothing
// about that.

// writeDepCheckModule seeds a module with two packages: a, and b which calls
// a. Changing a's API breaks b without touching a's own build.
func writeDepCheckModule(t *testing.T, work string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module depcheck\n\ngo 1.21\n",
		"a/a.go": "package a\n\nfunc Hello() string { return \"hi\" }\n",
		"b/b.go": "package b\n\nimport \"depcheck/a\"\n\nfunc Use() string { return a.Hello() }\n",
	}
	for rel, body := range files {
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const brokenA = "package a\n\nfunc Hello() int { return 1 }\n"

// TestDirect_ProjectCheckCatchesBrokenDependentPackage: the turn edits a
// package in a way that breaks a DIFFERENT package, and the whole-project
// check at finish catches it.
func TestDirect_ProjectCheckCatchesBrokenDependentPackage(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	writeDepCheckModule(t, work)
	cfg := directCfg(t, work, data, "change a.Hello")
	cfg.DisableAutoVerify = false // this test is about the check

	var mu sync.Mutex
	writes := 0
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		writes++
		switch writes {
		case 1:
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "a/a.go", "content": brokenA}), nil
		case 2:
			// The model believes it is done. The project check disagrees.
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
		case 3:
			// The refusal is in the transcript now: put the package back.
			return jsonAct(tools.ToolWriteFile, map[string]any{
				"path":    "a/a.go",
				"content": "package a\n\nfunc Hello() string { return \"hi\" }\n",
			}), nil
		default:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "fixed"}), nil
		}
	}}

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	d := NewDirect(o, ctl)
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(d.transcriptPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "finish REJECTED") {
		t.Fatalf("the finish must be refused while the project does not build:\n%s", text)
	}
	if !strings.Contains(text, "the whole project does not build") {
		t.Fatalf("the refusal must say what failed:\n%s", text)
	}
	// The diagnostics must name the package that broke — the one the model
	// never touched.
	if !strings.Contains(text, "b/b.go") {
		t.Fatalf("the refusal must name the broken dependent package:\n%s", text)
	}
	if ctl.summary != "fixed" {
		t.Fatalf("summary should be the final finish, got %q", ctl.summary)
	}
}

// TestDirect_ProjectCheckIgnoresPreExistingBreakage: a workspace that was
// already broken before the user typed anything must not be blamed on the
// turn, and the model must not be sent off to fix someone else's tree.
func TestDirect_ProjectCheckIgnoresPreExistingBreakage(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	writeDepCheckModule(t, work)
	// Somebody else's breakage: a syntax error in an unrelated package.
	if err := os.MkdirAll(filepath.Join(work, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "c", "c.go"), []byte("package c\n\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := directCfg(t, work, data, "add a comment to a.go")
	cfg.DisableAutoVerify = false

	var mu sync.Mutex
	finishes := 0
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		finishes++
		if finishes == 1 {
			return jsonAct(tools.ToolWriteFile, map[string]any{
				"path":    "a/a.go",
				"content": "package a\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n",
			}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	d := NewDirect(o, ctl)
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, _ := os.ReadFile(d.transcriptPath())
	if strings.Contains(string(raw), "finish REJECTED") {
		t.Fatalf("pre-existing breakage must not refuse the finish:\n%s", raw)
	}
	if ctl.summary != "done" {
		t.Fatalf("the turn should have finished on the first attempt, summary=%q", ctl.summary)
	}
}

// TestDirect_ProjectCheckSkippedWhenNothingWasEdited: a turn that only
// answered a question must not pay for a whole-project build.
func TestDirect_ProjectCheckSkippedWhenNothingWasEdited(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	writeDepCheckModule(t, work)
	cfg := directCfg(t, work, data, "what does this do?")
	cfg.DisableAutoVerify = false

	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "it greets"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, &testDirectCtl{})
	ran := 0
	d.projectCheck = func(ctx context.Context, cmds []string) tools.EditCheck {
		ran++
		return tools.EditCheck{Project: tools.ProjectGo, Commands: cmds}
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ran != 0 {
		t.Fatalf("the project check must not run when nothing was edited, ran=%d", ran)
	}
}
