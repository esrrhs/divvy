package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func TestParsePlanItems(t *testing.T) {
	items, err := parsePlanItems([]any{
		map[string]any{"step": "read the code", "status": "done"},
		map[string]any{"step": "implement", "status": "doing"},
		map[string]any{"step": "run tests", "status": "todo"},
		map[string]any{"step": "cleanup", "status": "skipped"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{planCompleted, planInProgress, planPending, planCancelled}
	for i, w := range want {
		if items[i].Status != w {
			t.Fatalf("item %d status = %q, want %q", i, items[i].Status, w)
		}
	}

	for name, raw := range map[string]any{
		"empty":           []any{},
		"not a list":      "do things",
		"empty step":      []any{map[string]any{"step": "  ", "status": "pending"}},
		"bad status":      []any{map[string]any{"step": "x", "status": "eventually"}},
		"too many active": []any{map[string]any{"step": "a", "status": "in_progress"}, map[string]any{"step": "b", "status": "in_progress"}},
	} {
		if _, err := parsePlanItems(raw); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

// TestDirectPlanGatesFinish covers the protocol: the model can declare a
// plan, but finish is rejected until every step is completed or deliberately
// cancelled. A rejected finish must not end the turn or lose the workspace.
func TestDirectPlanGatesFinish(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "do two things")
	tree := engine.NewTaskTree("t-plan", "root", "do two things")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	d.st = &directState{Version: 1, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "do two things"},
	}}

	plan := taggedAction{ID: "call-plan", Name: tools.ToolUpdatePlan, Args: map[string]any{
		"items": []any{
			map[string]any{"step": "write the code", "status": "completed"},
			map[string]any{"step": "run the tests", "status": "in_progress"},
		},
	}}
	finished, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{plan})
	if err != nil || finished {
		t.Fatalf("update_plan should not finish the turn: finished=%v err=%v", finished, err)
	}
	if len(d.st.Plan) != 2 || d.st.Plan[1].Status != planInProgress {
		t.Fatalf("plan not stored: %+v", d.st.Plan)
	}
	last := d.st.Messages[len(d.st.Messages)-1].Content
	if !strings.Contains(last, "Plan updated") || !strings.Contains(last, "run the tests") {
		t.Fatalf("model should see the stored plan, got %q", last)
	}

	finish := taggedAction{ID: "call-finish", Name: tools.ToolFinish, Args: map[string]any{"summary": "done"}}
	if finished, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{finish}); err != nil || finished {
		t.Fatalf("finish with an open step must be rejected: finished=%v err=%v", finished, err)
	}
	rejected := d.st.Messages[len(d.st.Messages)-1].Content
	if !strings.Contains(rejected, "finish rejected") || !strings.Contains(rejected, "run the tests") {
		t.Fatalf("finish rejection must name the open step, got %q", rejected)
	}

	// Cancelling is an explicit decision and unblocks finish just like
	// completing does; pending is an omission and does not.
	plan.Args = map[string]any{"items": []any{
		map[string]any{"step": "write the code", "status": "completed"},
		map[string]any{"step": "run the tests", "status": "cancelled"},
	}}
	if _, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{plan}); err != nil {
		t.Fatal(err)
	}
	if finished, summary, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{finish}); err != nil || !finished {
		t.Fatalf("cancelled steps should allow finish: finished=%v summary=%q err=%v", finished, summary, err)
	}
}

// TestDirectPlanInvalidUpdateKeepsPreviousPlan makes sure a malformed update
// is a normal tool error: the old plan stays intact so the model can repair
// instead of losing its list.
func TestDirectPlanInvalidUpdateKeepsPreviousPlan(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "goal")
	tree := engine.NewTaskTree("t-plan-bad", "root", "goal")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	d.st = &directState{Version: 1, Messages: []llm.Message{{Role: llm.RoleSystem, Content: "system"}}}
	good := taggedAction{ID: "p1", Name: tools.ToolUpdatePlan, Args: map[string]any{
		"items": []any{map[string]any{"step": "keep me", "status": "pending"}},
	}}
	if _, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{good}); err != nil {
		t.Fatal(err)
	}
	bad := taggedAction{ID: "p2", Name: tools.ToolUpdatePlan, Args: map[string]any{
		"items": []any{map[string]any{"step": "x", "status": "nonsense"}},
	}}
	if _, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{bad}); err != nil {
		t.Fatal(err)
	}
	if len(d.st.Plan) != 1 || d.st.Plan[0].Step != "keep me" {
		t.Fatalf("invalid update must not replace the plan: %+v", d.st.Plan)
	}
	last := d.st.Messages[len(d.st.Messages)-1].Content
	if !strings.Contains(last, "ERROR:") || !strings.Contains(last, "invalid") {
		t.Fatalf("invalid update should return a repair hint, got %q", last)
	}

	block := directPlanBlock(d.st.Plan)
	if !strings.Contains(block, "CURRENT PLAN") || !strings.Contains(block, "keep me") {
		t.Fatalf("plan block should carry the current list: %q", block)
	}
	if directPlanBlock(nil) != "" {
		t.Fatal("empty plan should add nothing to the system prompt")
	}
}
