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

// The post-edit check is the loop's "observe" step. These tests inject the
// check itself so they do not depend on run_bash being available on the host;
// the real command planning and execution are covered in pkg/tools.

func editCheckScript(rejectFirst bool) (func(context.Context, []string) tools.EditCheck, func() int) {
	var mu sync.Mutex
	calls := 0
	fn := func(ctx context.Context, paths []string) tools.EditCheck {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if rejectFirst && calls == 1 {
			return tools.EditCheck{
				Project:  tools.ProjectGo,
				Commands: []string{"fake-build ./pkg"},
				Failed:   true,
				Output:   "pkg/a.go:3:1: syntax error: unexpected newline",
			}
		}
		return tools.EditCheck{Project: tools.ProjectGo, Commands: []string{"fake-build ./pkg"}}
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	return fn, count
}

func bodyText(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDirect_EditCheckFeedsBackAndBlocksFinish: a failed check reaches the
// model in the same tool result, finish is refused while it still fails, and
// the turn completes once a later edit passes.
func TestDirect_EditCheckFeedsBackAndBlocksFinish(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "implement F")
	cfg.Goal = "implement F"
	cfg.DisableAutoVerify = false // this test is about the check

	var mu sync.Mutex
	writes := 0
	rechecks := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		body := bodyText(req)
		rejected := strings.Contains(body, "finish REJECTED")
		switch {
		case writes == 0:
			writes++
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "a.go", "content": "package a\n\nfunc F() {\n"}), nil
		case writes == 1 && !rejected:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
		case writes == 1 && rejected:
			writes++
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "a.go", "content": "package a\n\nfunc F() {}\n"}), nil
		default:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "fixed the syntax error"}), nil
		}
	}}

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	d := NewDirect(o, ctl)
	check, calls := editCheckScript(true)
	d.editCheck = check
	d.recheckEdits = func(ctx context.Context, cmds []string) tools.EditCheck {
		mu.Lock()
		rechecks++
		mu.Unlock()
		return tools.EditCheck{Project: tools.ProjectGo, Commands: cmds, Failed: true, Output: "pkg/a.go:3:1: still broken"}
	}

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls() != 2 {
		t.Fatalf("expected a check after each write (2), got %d", calls())
	}
	if rechecks != 1 {
		t.Fatalf("finish should re-run the failing check once, got %d", rechecks)
	}
	raw, err := os.ReadFile(d.transcriptPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"[auto-verify]", "FAILED", "syntax error", "finish REJECTED", "still broken"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(ctl.summary, "fixed") {
		t.Fatalf("summary should be the final finish, got %q", ctl.summary)
	}
}

// TestDirect_EditCheckSkipsMultipleActions: when a write fails its check, a
// finish planned in the same model reply is not executed.
func TestDirect_EditCheckSkipsMultipleActions(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "implement F")
	cfg.Goal = "implement F"
	cfg.DisableAutoVerify = false // this test is about the check
	cfg.NativeTools = true

	var mu sync.Mutex
	step := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		step++
		if step == 1 {
			return nativeResp(
				nativeTC("c1", tools.ToolWriteFile, map[string]any{"path": "a.go", "content": "package a\n\nfunc F() {\n"}),
				nativeTC("c2", tools.ToolFinish, map[string]any{"summary": "should not run"}),
			), nil
		}
		return nativeResp(nativeTC("c3", tools.ToolFinish, map[string]any{"summary": "second attempt"})), nil
	}}

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, &testDirectCtl{})
	d.editCheck = func(ctx context.Context, paths []string) tools.EditCheck {
		return tools.EditCheck{Project: tools.ProjectGo, Commands: []string{"fake-build"}, Failed: true, Output: "boom"}
	}
	// The gate re-runs and fails once more, then the model finishes anyway in
	// this test (its summary is what proves the first finish was skipped).
	d.recheckEdits = func(ctx context.Context, cmds []string) tools.EditCheck {
		return tools.EditCheck{Project: tools.ProjectGo, Commands: cmds}
	}
	ctl := &testDirectCtl{}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ctl.summary == "should not run" {
		t.Fatalf("finish from the same reply as a failed edit must be skipped, summary=%q", ctl.summary)
	}
	raw, _ := os.ReadFile(filepath.Join(data, cfg.SessionID+".direct.json"))
	if !strings.Contains(string(raw), "Not executed") {
		t.Fatalf("skipped action should be reported to the model:\n%s", raw)
	}
}

// TestDirect_DisableAutoVerify: the knob removes the check entirely (for hosts
// where a build after every edit is too expensive).
func TestDirect_DisableAutoVerify(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "implement F")
	cfg.Goal = "implement F"
	cfg.DisableAutoVerify = false // this test is about the check
	cfg.DisableAutoVerify = true

	var mu sync.Mutex
	writes := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if writes == 0 {
			writes++
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "a.go", "content": "package a\n\nfunc F() {\n"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, &testDirectCtl{})
	called := false
	d.editCheck = func(ctx context.Context, paths []string) tools.EditCheck {
		called = true
		return tools.EditCheck{}
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if called {
		t.Fatal("DisableAutoVerify should skip the post-edit check")
	}
}

// TestDirect_VerifyGateYieldsAfterRepeatedRejections: an unfixable check (wrong
// toolchain, missing network) must not trap the turn forever; after the second
// refusal the turn ends with the failure disclosed instead of burning the step
// budget.
func TestDirect_VerifyGateYieldsAfterRepeatedRejections(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "implement F")
	cfg.Goal = "implement F"
	cfg.DisableAutoVerify = false

	var mu sync.Mutex
	writes, finishes := 0, 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if writes == 0 {
			writes++
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "a.go", "content": "package a\n\nfunc F() {}\n"}), nil
		}
		finishes++
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "giving up"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	d := NewDirect(o, ctl)
	d.editCheck = func(ctx context.Context, paths []string) tools.EditCheck {
		return tools.EditCheck{Project: tools.ProjectGo, Commands: []string{"fake-build"}, Failed: true, Output: "toolchain broken"}
	}
	d.recheckEdits = func(ctx context.Context, cmds []string) tools.EditCheck {
		return tools.EditCheck{Project: tools.ProjectGo, Commands: cmds, Failed: true, Output: "toolchain broken"}
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if finishes != directVerifyMaxRejects+1 {
		t.Fatalf("expected %d finish attempts (reject, reject, allow), got %d", directVerifyMaxRejects+1, finishes)
	}
	if ctl.summary != "giving up" {
		t.Fatalf("the waived finish should still end the turn, summary=%q", ctl.summary)
	}
}
