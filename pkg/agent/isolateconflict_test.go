package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// sharedFilePlan has both leaves declare and write the same output file, which
// the plan checker flags as an isolated-run conflict.
const sharedFilePlan = `{
  "is_atomic": false,
  "reason": "two leaves touching one file",
  "subtasks": [
    {
      "id": "alpha",
      "title": "Write shared file (alpha)",
      "description": "Write shared.txt",
      "type": "LEAF",
      "contract": {"outputs": ["shared.txt"]},
      "dod": {"commands": ["test -f shared.txt"]}
    },
    {
      "id": "beta",
      "title": "Write shared file (beta)",
      "description": "Write shared.txt",
      "type": "LEAF",
      "contract": {"outputs": ["shared.txt"]},
      "dod": {"commands": ["test -f shared.txt"]}
    }
  ]
}`

// undeclaredSharedFilePlan writes the same file from both leaves without
// declaring it as an output. The scheduler cannot serialize these (there is no
// claim to take), so the mirror conflict check is the last line of defence —
// this is the realistic residual case for weak models that forget to fill in
// contract.outputs.
const undeclaredSharedFilePlan = `{
  "is_atomic": false,
  "reason": "two leaves with undeclared shared writes",
  "subtasks": [
    {
      "id": "alpha",
      "title": "Write shared file (alpha)",
      "description": "Write shared.txt",
      "type": "LEAF",
      "dod": {"commands": ["test -f shared.txt"]}
    },
    {
      "id": "beta",
      "title": "Write shared file (beta)",
      "description": "Write shared.txt",
      "type": "LEAF",
      "dod": {"commands": ["test -f shared.txt"]}
    }
  ]
}`

// TestOrchestrator_IsolatedParallelSharedFileNoLostUpdate is the end-to-end
// regression test for the mirror lost-update defect. Two leaves run
// concurrently under -isolate and both write the same file, but neither
// declares it, so the scheduler cannot serialize them. The old merge let
// whichever finished last silently win, discarding the other leaf's verified
// change; now the stale merge is rejected and the leaf re-snapshots.
func TestOrchestrator_IsolatedParallelSharedFileNoLostUpdate(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	// Both leaves rendezvous on their first worker call so their mirrors are
	// guaranteed to come from the same base state.
	var startOnce sync.Once
	bothStarted := make(chan struct{})

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: undeclaredSharedFilePlan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			// A retried leaf is told to reconcile with the sibling's version.
			reconciling := strings.Contains(user, "already changed those file")

			leafID := ""
			switch {
			case strings.Contains(user, "Task ID: alpha"):
				leafID = "alpha"
			case strings.Contains(user, "Task ID: beta"):
				leafID = "beta"
			default:
				return jsonAction("finish", map[string]string{"summary": "unknown"}), nil
			}

			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": leafID + " done"}), nil
			}
			if reconciling {
				// Read the sibling's merged version before re-applying.
				return jsonAction("read_file", map[string]string{"path": "shared.txt"}), nil
			}
			startOnce.Do(func() { close(bothStarted) })
			select {
			case <-bothStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return jsonAction("write_file", map[string]string{
				"path":    "shared.txt",
				"content": "written by " + leafID,
			}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_merge_conflict"
	cfg.Goal = "two leaves writing the same file"
	cfg.Stream = false
	cfg.Parallel = 2
	cfg.Isolate = true
	cfg.MaxRetries = 3

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	runErr := o.Run(context.Background())

	// Both leaves must end up COMPLETED: the conflicted one recovers by
	// re-snapshotting rather than clobbering or failing outright.
	for _, leaf := range o.tree.Leaves() {
		if leaf.State != models.TaskStateCompleted {
			t.Fatalf("leaf %s state = %s, want COMPLETED (err=%v):\n%s",
				leaf.ID, leaf.State, runErr, o.tree.RenderVisualTree())
		}
	}

	// The delivered file must contain one leaf's verified write — never a
	// blend and never an empty file.
	raw, err := os.ReadFile(filepath.Join(work, "shared.txt"))
	if err != nil {
		t.Fatalf("shared.txt missing after run (err=%v):\n%s", runErr, o.tree.RenderVisualTree())
	}
	got := string(raw)
	if !strings.Contains(got, "written by alpha") && !strings.Contains(got, "written by beta") {
		t.Fatalf("shared.txt contains neither leaf's verified write: %q\n%s", got, o.tree.RenderVisualTree())
	}

	// The distinguishing signal: with conflict detection the contended merge
	// is rejected and recorded, and the leaf recovers. Without it the second
	// merge silently overwrites its sibling and no conflict is ever seen.
	sawConflict := false
	for _, leaf := range o.tree.Leaves() {
		for _, rec := range leaf.ErrorHistory {
			if strings.Contains(rec.Error, "merge conflict") {
				sawConflict = true
			}
		}
	}
	if !sawConflict {
		t.Fatalf("expected a recorded merge conflict for two isolated leaves writing the same file; "+
			"none means a stale mirror overwrote a sibling silently\n%s", o.tree.RenderVisualTree())
	}
	t.Logf("run err=%v; shared.txt=%q; conflict detected and recovered", runErr, got)
}

// TestOrchestrator_ConflictingOutputsSerialize proves the scheduler holds back
// a leaf whose declared output is claimed by an in-flight sibling, instead of
// running both concurrently and racing on the same file.
func TestOrchestrator_ConflictingOutputsSerialize(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var mu sync.Mutex
	var active int
	var maxActive int

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: sharedFilePlan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			leafID := ""
			switch {
			case strings.Contains(user, "Task ID: alpha"):
				leafID = "alpha"
			case strings.Contains(user, "Task ID: beta"):
				leafID = "beta"
			default:
				return jsonAction("finish", map[string]string{"summary": "unknown"}), nil
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": leafID + " done"}), nil
			}
			// Hold the leaf in its worker phase long enough to observe overlap.
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return jsonAction("write_file", map[string]string{
				"path":    "shared.txt",
				"content": "written by " + leafID,
			}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_claim_serialize"
	cfg.Goal = "two leaves writing the same file"
	cfg.Stream = false
	cfg.Parallel = 2
	cfg.Isolate = true
	cfg.MaxRetries = 2

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("run failed: %v\n%s", err, o.tree.RenderVisualTree())
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}
	mu.Lock()
	got := maxActive
	mu.Unlock()
	// Both leaves declare shared.txt, so the claim must keep them from
	// overlapping regardless of -parallel 2.
	if got != 1 {
		t.Fatalf("conflicting leaves overlapped (max concurrent workers = %d, want 1)", got)
	}
	// Sequential execution means no merge conflict arises at all.
	for _, leaf := range o.tree.Leaves() {
		for _, rec := range leaf.ErrorHistory {
			if strings.Contains(rec.Error, "merge conflict") {
				t.Fatalf("leaf %s hit a merge conflict; the scheduler should have serialized it: %s",
					leaf.ID, rec.Error)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(work, "shared.txt")); err != nil {
		t.Fatalf("shared.txt missing: %v", err)
	}
}

// TestOrchestrator_IsolatedConflictRecordedOnNode verifies the conflict is
// surfaced on the node's persistent error history, so an operator can see why
// a leaf needed extra attempts.
func TestOrchestrator_IsolatedConflictRecordedOnNode(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	// A single leaf that rewrites a file another process keeps changing is
	// hard to script deterministically, so this covers the observable
	// contract instead: when publishLeaf returns a merge conflict, the node
	// records the error and stays retryable.
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}

	o, err := NewFromGoal(func() Config {
		cfg := DefaultConfig()
		cfg.WorkDir = work
		cfg.DataDir = data
		cfg.SessionID = "test_conflict_record"
		cfg.Goal = "write f.txt"
		cfg.Stream = false
		return cfg
	}(), &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: `{"is_atomic": true, "reason": "one file", "contract": {"outputs": ["f.txt"]}, "dod": {"commands": ["test -f f.txt"]}, "subtasks": []}`}, nil
		},
	}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	node, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		t.Fatal("root node not found")
	}
	o.recordNodeError(node.ID, "merge conflict on shared.txt: sibling changed the same file")
	live, ok := o.tree.CloneNode(node.ID)
	if !ok {
		t.Fatal("node vanished")
	}
	if live.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", live.RetryCount)
	}
	if len(live.ErrorHistory) != 1 {
		t.Fatalf("ErrorHistory = %d entries, want 1", len(live.ErrorHistory))
	}
	if !strings.Contains(live.ErrorHistory[0].Error, "merge conflict") {
		t.Fatalf("error not recorded: %q", live.ErrorHistory[0].Error)
	}
}
