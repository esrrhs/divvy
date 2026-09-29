package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
	"github.com/esrrhs/go_llm_engine/pkg/models"
)

const twoLeafPlan = `{
  "is_atomic": false,
  "reason": "two independent files",
  "subtasks": [
    {
      "id": "alpha",
      "title": "Write alpha file",
      "description": "Write a.txt containing alpha",
      "type": "LEAF",
      "contract": {"outputs": ["a.txt"]},
      "dod": {"commands": ["test -f a.txt"]}
    },
    {
      "id": "beta",
      "title": "Write beta file",
      "description": "Write b.txt containing beta",
      "type": "LEAF",
      "contract": {"outputs": ["b.txt"]},
      "dod": {"commands": ["test -f b.txt"]}
    }
  ]
}`

// twoLeafWorkerHandle scripts the two leaves: each writes its file then finishes.
// onWorker is invoked for every worker call.
func twoLeafWorkerHandle(onWorker func(req llm.Request)) func(context.Context, llm.Request) (*llm.Response, error) {
	return func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{Content: twoLeafPlan}, nil
		}
		if onWorker != nil {
			onWorker(req)
		}
		user := ""
		if len(req.Messages) > 1 {
			user = req.Messages[1].Content
		}
		switch {
		case strings.Contains(user, "Task ID: alpha"):
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "alpha done"}), nil
			}
			return jsonAction("write_file", map[string]string{"path": "a.txt", "content": "alpha"}), nil
		case strings.Contains(user, "Task ID: beta"):
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "beta done"}), nil
			}
			return jsonAction("write_file", map[string]string{"path": "b.txt", "content": "beta"}), nil
		default:
			return jsonAction("finish", map[string]string{"summary": "unknown task"}), nil
		}
	}
}

func reqHasToolResult(req llm.Request) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
			return true
		}
	}
	return false
}

// TestOrchestrator_ParallelLeavesOverlap proves two independent leaves execute
// concurrently with Parallel=2: each worker's first LLM call only completes
// after the other worker has started.
func TestOrchestrator_ParallelLeavesOverlap(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	startedA := make(chan struct{})
	startedB := make(chan struct{})
	var onceA, onceB sync.Once
	var overlap atomic.Bool

	waitFor := func(wc chan struct{}, other chan struct{}, once *sync.Once) {
		once.Do(func() { close(wc) })
		select {
		case <-other:
			overlap.Store(true)
		case <-time.After(10 * time.Second):
		}
	}

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: twoLeafPlan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			switch {
			case strings.Contains(user, "Task ID: alpha"):
				if reqHasToolResult(req) {
					return jsonAction("finish", map[string]string{"summary": "alpha done"}), nil
				}
				waitFor(startedA, startedB, &onceA)
				return jsonAction("write_file", map[string]string{"path": "a.txt", "content": "alpha"}), nil
			case strings.Contains(user, "Task ID: beta"):
				if reqHasToolResult(req) {
					return jsonAction("finish", map[string]string{"summary": "beta done"}), nil
				}
				waitFor(startedB, startedA, &onceB)
				return jsonAction("write_file", map[string]string{"path": "b.txt", "content": "beta"}), nil
			default:
				return jsonAction("finish", map[string]string{"summary": "unknown"}), nil
			}
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_parallel"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.Parallel = 2
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}
	if !overlap.Load() {
		t.Fatal("leaves did not overlap; parallel execution did not happen")
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
}

// TestOrchestrator_UsageTracked checks token accounting across decompose and worker calls.
func TestOrchestrator_UsageTracked(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{
					Content: `{"is_atomic": true, "reason": "single file", "contract": {"outputs": ["a.txt"]}, "dod": {"commands": ["test -f a.txt"]}, "subtasks": []}`,
					Usage:   llm.Usage{PromptTokens: 7, CompletionTokens: 3},
				}, nil
			}
			if reqHasToolResult(req) {
				return &llm.Response{
					Content: `{"thought":"done","action":"finish","args":{"summary":"ok"}}`,
					Usage:   llm.Usage{PromptTokens: 7, CompletionTokens: 3},
				}, nil
			}
			return &llm.Response{
				Content: `{"thought":"write","action":"write_file","args":{"path":"a.txt","content":"alpha"}}`,
				Usage:   llm.Usage{PromptTokens: 7, CompletionTokens: 3},
			}, nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_usage"
	cfg.Goal = "write one file"
	cfg.Stream = false
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}

	wantCalls := len(client.Requests)
	u, calls := o.usage.Total()
	if calls != wantCalls {
		t.Fatalf("tracked %d calls, client saw %d", calls, wantCalls)
	}
	if u.PromptTokens != 7*calls || u.CompletionTokens != 3*calls || u.TotalTokens != 10*calls {
		t.Fatalf("usage mismatch: %+v for %d calls", u, calls)
	}

	keys, kinds := o.usage.SortedKinds()
	if len(keys) != 2 || kinds["decompose"].TotalTokens != 10 || kinds["worker"].TotalTokens != 20 {
		t.Fatalf("per-kind usage unexpected: %v", kinds)
	}

	root := o.tree.GetRoot()
	if root.TokenUsage.Calls != wantCalls || root.TokenUsage.TotalTokens != 10*wantCalls {
		t.Fatalf("node usage: %+v, want %d calls / %d tokens", root.TokenUsage, wantCalls, 10*wantCalls)
	}
	if got := o.tree.TotalTokenUsage(); got.Calls != wantCalls || got.TotalTokens != 10*wantCalls {
		t.Fatalf("tree usage: %+v", got)
	}
}

// TestOrchestrator_PlanOnlyThenResume runs -plan without executing, then resumes.
func TestOrchestrator_PlanOnlyThenResume(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var workerCalls atomic.Int32
	client := &llm.ScriptedClient{
		Handle: twoLeafWorkerHandle(func(req llm.Request) { workerCalls.Add(1) }),
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_plan"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workerCalls.Load() != 0 {
		t.Fatalf("plan mode executed %d worker calls", workerCalls.Load())
	}
	if _, err := os.Stat(filepath.Join(work, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("plan mode wrote workspace files")
	}
	for _, id := range []string{"alpha", "beta"} {
		n, ok := o.tree.CloneNode(id)
		if !ok {
			t.Fatalf("missing node %s", id)
		}
		if n.Type != models.NodeTypeLeaf || n.State != models.TaskStatePending {
			t.Fatalf("node %s type=%s state=%s, want leaf/pending", id, n.Type, n.State)
		}
	}
	_, total, _ := o.tree.GetLeafProgress()
	if total != 2 {
		t.Fatalf("plan produced %d leaves, want 2", total)
	}

	// Resume the planned session and execute it.
	resumeClient := &llm.ScriptedClient{Handle: twoLeafWorkerHandle(nil)}
	o2, err := Load(cfg, resumeClient, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o2.sched.IsComplete() {
		t.Fatalf("resume not complete:\n%s", o2.tree.RenderVisualTree())
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Fatalf("missing %s after resume: %v", f, err)
		}
	}
}
