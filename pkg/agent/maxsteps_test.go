package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// passingLeafPlan is a leaf whose DoD trivially passes. Under the old
// behaviour a worker that never called finish still walked into verification,
// and this DoD marked an unfinished leaf COMPLETED — the bug this test pins.
const passingLeafPlan = `{
  "is_atomic": true,
  "reason": "single leaf",
  "contract": {"outputs": ["out.txt"]},
  "dod": {"commands": ["true"]},
  "subtasks": []
}`

// TestOrchestrator_MaxStepsIsNotSuccess is the regression test for the dead
// "max steps reached" signal. The worker burns every step without calling
// finish, so nothing is known to be done; the attempt must be treated as a
// failure even though the DoD would pass.
func TestOrchestrator_MaxStepsIsNotSuccess(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var sawBudgetHint atomic.Bool
	var requests atomic.Int64
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: passingLeafPlan}, nil
			}
			requests.Add(1)
			for _, m := range req.Messages {
				if strings.Contains(m.Content, "[budget]") {
					sawBudgetHint.Store(true)
				}
			}
			// Never finishes: keeps editing until the step budget runs out.
			return jsonAction("write_file", map[string]string{
				"path":    "out.txt",
				"content": "partial\n",
			}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_max_steps"
	cfg.Goal = "write a file"
	cfg.Stream = false
	cfg.MaxSteps = 3
	cfg.MaxRetries = 0
	cfg.MaxStall = 2       // both attempts fail identically (out of steps)
	cfg.MaxRedecompose = 0 // so the leaf is failed instead of re-split
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = o.Run(ctx)

	leaf, _ := o.tree.CloneNode(o.tree.RootID)
	if leaf.State == models.TaskStateCompleted {
		t.Fatalf("leaf must not be COMPLETED when the worker never finished:\n%s", o.tree.RenderVisualTree())
	}
	if !strings.Contains(leaf.ErrorMsg, "max tool calls") {
		t.Fatalf("failure should name the step budget, got: %q", leaf.ErrorMsg)
	}
	if !sawBudgetHint.Load() {
		t.Fatal("worker should have been told how many tool calls it had left")
	}
}

// TestStepBudgetMessages covers the reminder itself: it appears only in the
// last few steps, states the real count, and never mutates the history it was
// derived from (a stale count in history would mislead every later step).
func TestStepBudgetMessages(t *testing.T) {
	o := &Orchestrator{}
	msgs := []llm.Message{{Role: llm.RoleSystem}, {Role: llm.RoleUser}}

	if got := o.stepBudgetMessages(msgs, 1, 8); len(got) != 2 {
		t.Fatalf("no hint expected early in the budget, got %d messages", len(got))
	}
	got := o.stepBudgetMessages(msgs, 4, 8) // 5 left → inside window of 5
	if len(got) != 3 {
		t.Fatalf("hint expected in the last 5 steps, got %d messages", len(got))
	}
	if !strings.Contains(got[2].Content, "5 tool call(s) left") {
		t.Fatalf("hint should state the remaining count: %q", got[2].Content)
	}
	// Last step: force finish wording.
	last := o.stepBudgetMessages(msgs, 8, 8)
	if !strings.Contains(last[2].Content, "LAST tool call") {
		t.Fatalf("last step should force finish: %q", last[2].Content)
	}
	if len(msgs) != 2 {
		t.Fatalf("stepBudgetMessages mutated the caller's history: %d", len(msgs))
	}
}

// TestRunWorker_InvalidJSONAbortsEarly pins the weak-model guard: consecutive
// empty/unparseable replies must abort the attempt before MaxSteps is burned.
func TestRunWorker_InvalidJSONAbortsEarly(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: "sure, I will help with that"}, nil
	}}
	o, sb := newWorkerOrchestrator(t, client)
	o.cfg.MaxSteps = 20

	_, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if err == nil {
		t.Fatal("expected invalid-JSON abort")
	}
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("err = %v, want ErrInvalidJSON", err)
	}
	if n := len(client.Requests); n != maxInvalidJSONRounds {
		t.Fatalf("worker calls = %d, want %d (must not burn MaxSteps)", n, maxInvalidJSONRounds)
	}
}
