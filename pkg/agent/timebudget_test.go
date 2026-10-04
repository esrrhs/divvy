package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// failingLeafPlan is a single leaf whose DoD can never pass.
const failingLeafPlan = `{
  "is_atomic": true,
  "reason": "single leaf",
  "contract": {"outputs": ["never.txt"]},
  "dod": {"commands": ["test -f never.txt"]},
  "subtasks": []
}`

// TestOrchestrator_LeafTimeBudgetStopsInfiniteRetries is the regression test
// for the unbounded retry loop: with MaxRetries=0 a leaf whose DoD can never
// pass used to retry forever, spending tokens on a goal that cannot land.
// MaxElapsed must cut it off and fail the node with a clear reason.
func TestOrchestrator_LeafTimeBudgetStopsInfiniteRetries(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var workerCalls atomic.Int64
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: failingLeafPlan}, nil
			}
			workerCalls.Add(1)
			// The leaf "does work" but never produces the file its DoD wants.
			return jsonAction("write_file", map[string]string{
				"path":    "wrong.txt",
				"content": "not the required file",
			}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_time_budget"
	cfg.Goal = "write a library file"
	cfg.Stream = false
	cfg.MaxRetries = 0 // unlimited attempts — only the time budget can stop this
	cfg.MaxElapsed = 900 * time.Millisecond
	cfg.RetryMinInterval = 10 * time.Millisecond
	cfg.RetryMaxInterval = 20 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	done := make(chan error, 1)
	go func() { done <- o.Run(context.Background()) }()

	select {
	case runErr := <-done:
		// The run must terminate on its own with a time-budget reason.
		if runErr == nil {
			t.Fatalf("run should not succeed; worker calls = %d\n%s",
				workerCalls.Load(), o.tree.RenderVisualTree())
		}
		if !strings.Contains(runErr.Error(), "time budget") && !strings.Contains(runErr.Error(), "max-elapsed") {
			t.Fatalf("run error should mention the time budget, got: %v", runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("run did not terminate; the retry loop is still unbounded (worker calls = %d)",
			workerCalls.Load())
	}

	// The run terminated on its own, which is the property that matters: with
	// MaxRetries=0 only the wall-clock budget can end this loop.
	if n := workerCalls.Load(); n == 0 {
		t.Fatal("worker never ran; the test did not exercise the retry loop")
	}
	if o.sched.IsComplete() {
		t.Fatalf("root should not be complete:\n%s", o.tree.RenderVisualTree())
	}
}

// TestOrchestrator_TimeBudgetDisabledWhenZero verifies 0 keeps the previous
// unlimited-retry behaviour.
func TestOrchestrator_TimeBudgetDisabledWhenZero(t *testing.T) {
	o := &Orchestrator{
		cfg:       Config{MaxElapsed: 0},
		leafStart: map[string]time.Time{},
		log:       SilentLogger(),
	}
	if o.deadlineExceeded("x", 1) {
		t.Fatal("MaxElapsed=0 must never trip the deadline")
	}
}

// TestOrchestrator_DeadlineExceededUsesLeafStart covers the guard in isolation:
// the budget is measured from the leaf's first attempt.
func TestOrchestrator_DeadlineExceededUsesLeafStart(t *testing.T) {
	o := &Orchestrator{
		cfg:       Config{MaxElapsed: 50 * time.Millisecond},
		leafStart: map[string]time.Time{"a": time.Now().Add(-time.Second)},
		log:       SilentLogger(),
	}
	if !o.deadlineExceeded("a", 3) {
		t.Fatal("a leaf started well beyond MaxElapsed should be over budget")
	}
	// An untracked leaf (never started) has no budget to exceed.
	if o.deadlineExceeded("unknown", 1) {
		t.Fatal("an untracked leaf should not be considered over budget")
	}
}

// TestOrchestrator_RetryOrGiveUpRespectsDeadline verifies the deadline is
// consulted before MaxRetries, and that it fails the node rather than leaving
// it pending to spin.
func TestOrchestrator_RetryOrGiveUpRespectsDeadline(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: failingLeafPlan}, nil
		},
	}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_retry_gate"
	cfg.Goal = "write a library file"
	cfg.Stream = false
	cfg.MaxElapsed = time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	// Build a leaf node to exercise the gate against.
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		t.Fatal("root not found")
	}
	if derr := o.decompose(context.Background(), root); derr != nil {
		t.Fatalf("decompose: %v", derr)
	}
	leaves := o.tree.Leaves()
	if len(leaves) != 1 {
		t.Fatalf("expected one leaf, got %d", len(leaves))
	}
	leaf := leaves[0]
	o.leafMu.Lock()
	o.leafStart[leaf.ID] = time.Now().Add(-time.Hour)
	o.leafMu.Unlock()

	err = o.retryOrGiveUp(context.Background(), leaf, 5, "verification failed")
	if err == nil {
		t.Fatal("expected the run to surface a failure once the budget was spent")
	}
	if !strings.Contains(err.Error(), "time budget") {
		t.Fatalf("error should name the time budget, got: %v", err)
	}
	live, ok := o.tree.CloneNode(leaf.ID)
	if !ok {
		t.Fatal("leaf vanished")
	}
	if live.State != models.TaskStateFailed {
		t.Fatalf("leaf state = %s, want FAILED (a pending node would spin forever)", live.State)
	}
	// A time-budget give-up must not trigger a re-split: the leaf was never
	// proven too large, and re-decomposing would restart the clock.
	if live.Type != models.NodeTypeLeaf {
		t.Fatalf("leaf type = %s, want LEAF (time budget must not re-split)", live.Type)
	}
}
