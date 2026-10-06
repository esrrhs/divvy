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
	"github.com/esrrhs/divvy/pkg/tools"
)

// Atomic plan: the root node converts straight into the single leaf (id
// "root"). A non-atomic plan with one subtask is rejected by applyDecompose,
// which would make the decomposer retry forever.
const approvalPlan = `{
  "is_atomic": true,
  "reason": "one file",
  "contract": {"outputs": ["out.txt"]},
  "dod": {"commands": ["test -f out.txt"]},
  "subtasks": []
}`

// approvalScript writes content "first" on the first attempt and, after a
// human rejection (visible as prior-error in the user message), rewrites it
// with "second".
func approvalScript(t *testing.T) llm.Client {
	t.Helper()
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: approvalPlan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			if !strings.Contains(user, "Task ID: root") {
				return jsonAction("finish", map[string]string{"summary": "unknown"}), nil
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			}
			content := "first"
			if strings.Contains(user, "REVIEWED AND REJECTED") {
				content = "second"
			}
			return jsonAction("write_file", map[string]string{"path": "out.txt", "content": content}), nil
		},
	}
}

func approvalCfg(work, data string) Config {
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_leaf_approval"
	cfg.Goal = "write one file"
	cfg.Stream = false
	cfg.Isolate = true
	cfg.Parallel = 1
	cfg.MaxRetries = 3
	// Keep tests fast and bounded: a broken script must fail in seconds, not
	// spin through the default backoff/decompose loops.
	cfg.DecomposeTries = 2
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond
	return cfg
}

func TestManualApproval_RequiresIsolate(t *testing.T) {
	cfg := approvalCfg(t.TempDir(), t.TempDir())
	cfg.Isolate = false
	cfg.LeafApproval = LeafApprovalManual
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "requires isolate") {
		t.Fatalf("expected isolate-required error, got %v", err)
	}
}

func TestManualApproval_ApproveMerges(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	cfg := approvalCfg(work, data)
	cfg.LeafApproval = LeafApprovalManual

	o, err := NewFromGoal(cfg, approvalScript(t), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	var seen []LeafApprovalRequest
	var mu sync.Mutex
	o.SetLeafApprovalHook(func(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		return LeafApprovalDecision{Approved: true}, nil
	})

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("approval hook called %d times, want 1", len(seen))
	}
	if len(seen[0].Changes) != 1 || seen[0].Changes[0].Path != "out.txt" {
		t.Fatalf("unexpected changes: %+v", seen[0].Changes)
	}
	if seen[0].Changes[0].Status != tools.ChangeAdded || seen[0].Changes[0].NewContent != "first" {
		t.Fatalf("change payload wrong: %+v", seen[0].Changes[0])
	}
	if got, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(got) != "first" {
		t.Fatalf("approved change not merged: %q %v", got, err)
	}
	leaf, _ := o.tree.CloneNode(o.tree.RootID)
	if leaf == nil || leaf.State != models.TaskStateCompleted {
		t.Fatalf("leaf state = %+v", leaf)
	}
}

func TestManualApproval_RejectRedoesAndApproves(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	cfg := approvalCfg(work, data)
	cfg.SessionID = "test_leaf_approval_reject"
	cfg.LeafApproval = LeafApprovalManual

	o, err := NewFromGoal(cfg, approvalScript(t), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	calls := 0
	var firstReq LeafApprovalRequest
	o.SetLeafApprovalHook(func(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error) {
		calls++
		if calls == 1 {
			firstReq = req
			return LeafApprovalDecision{Approved: false, Comment: "must say second"}, nil
		}
		return LeafApprovalDecision{Approved: true}, nil
	})

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("hook calls = %d, want 2 (reject then approve)", calls)
	}
	if len(firstReq.Changes) != 1 || firstReq.Changes[0].NewContent != "first" {
		t.Fatalf("first review did not show the rejected content: %+v", firstReq.Changes)
	}
	if got, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(got) != "second" {
		t.Fatalf("workdir must hold the re-done approved content, got %q %v", got, err)
	}
	leaf, _ := o.tree.CloneNode(o.tree.RootID)
	if leaf == nil || leaf.State != models.TaskStateCompleted {
		t.Fatalf("leaf state = %+v", leaf)
	}
	if leaf.RetryCount < 1 {
		t.Fatalf("rejection should count as an attempt, retries = %d", leaf.RetryCount)
	}
}

func TestAutoApproval_NeverInvokesHook(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	cfg := approvalCfg(work, data) // default auto

	o, err := NewFromGoal(cfg, approvalScript(t), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	called := 0
	o.SetLeafApprovalHook(func(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error) {
		called++
		return LeafApprovalDecision{Approved: true}, nil
	})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if called != 0 {
		t.Fatalf("auto mode must not invoke the approval hook, got %d", called)
	}
}
