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

const failingAlphaPlan = `{
  "is_atomic": false,
  "reason": "two independent files",
  "subtasks": [
    {
      "id": "alpha",
      "title": "Write alpha file",
      "description": "Write a.txt containing good",
      "type": "LEAF",
      "contract": {"outputs": ["a.txt"]},
      "dod": {"commands": ["grep -q good a.txt"]}
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

// TestOrchestrator_IsolateRollbackOnFailure checks that a leaf whose attempts
// keep failing verification never leaks its files into the shared workspace,
// while a sibling that succeeds is merged normally.
func TestOrchestrator_IsolateRollbackOnFailure(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: failingAlphaPlan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			switch {
			case strings.Contains(user, "Task ID: alpha"):
				if reqHasToolResult(req) {
					return jsonAction("finish", map[string]string{"summary": "alpha claims done"}), nil
				}
				return jsonAction("write_file", map[string]string{"path": "a.txt", "content": "bad"}), nil
			case strings.Contains(user, "Task ID: beta"):
				if reqHasToolResult(req) {
					return jsonAction("finish", map[string]string{"summary": "beta done"}), nil
				}
				return jsonAction("write_file", map[string]string{"path": "b.txt", "content": "beta"}), nil
			default:
				return jsonAction("finish", map[string]string{"summary": "unknown"}), nil
			}
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_isolate_rollback"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.Isolate = true
	cfg.MaxRetries = 1
	cfg.MaxRedecompose = 0 // fail the leaf instead of re-splitting it

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	runErr := o.Run(context.Background())
	if runErr == nil || !strings.Contains(runErr.Error(), "root task failed") {
		t.Fatalf("expected root failure, got %v", runErr)
	}

	if _, err := os.Stat(filepath.Join(work, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("failed leaf's files leaked into the shared workspace")
	}
	if got, err := os.ReadFile(filepath.Join(work, "b.txt")); err != nil || string(got) != "beta" {
		t.Fatalf("successful sibling not merged: %q %v", got, err)
	}
	beta, ok := o.tree.CloneNode("beta")
	if !ok || beta.State != models.TaskStateCompleted {
		t.Fatalf("beta state wrong: %+v", beta)
	}
	if !strings.Contains(beta.ResultSummary, "Merged files: b.txt") {
		t.Fatalf("beta summary missing merge record: %q", beta.ResultSummary)
	}
}

// TestOrchestrator_IsolateMergesOnSuccess checks the happy path: an isolated
// leaf's verified output lands in the shared workspace with a merge record.
func TestOrchestrator_IsolateMergesOnSuccess(t *testing.T) {
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
				}, nil
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			}
			return jsonAction("write_file", map[string]string{"path": "a.txt", "content": "content"}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_isolate_merge"
	cfg.Goal = "write one file"
	cfg.Stream = false
	cfg.Isolate = true
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(work, "a.txt"))
	if err != nil || string(got) != "content" {
		t.Fatalf("merged file wrong: %q %v", got, err)
	}
	root := o.tree.GetRoot()
	if !strings.Contains(root.ResultSummary, "Merged files: a.txt") {
		t.Fatalf("summary missing merge record: %q", root.ResultSummary)
	}
}

// TestOrchestrator_ParallelIsolatedOverlap runs the two independent leaves
// concurrently in mirrors and verifies both merge back into the workspace.
func TestOrchestrator_ParallelIsolatedOverlap(t *testing.T) {
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
	cfg.SessionID = "test_parallel_isolate"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.Parallel = 2
	cfg.Isolate = true
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
		t.Fatal("leaves did not overlap")
	}
	for name, want := range map[string]string{"a.txt": "alpha", "b.txt": "beta"} {
		got, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
}
