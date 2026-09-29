package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
	"github.com/esrrhs/go_llm_engine/pkg/models"
)

// weakPlan has a leaf with placeholder verification and no outputs, plus two
// siblings claiming the same output file.
const weakPlan = `{
  "is_atomic": false,
  "reason": "split",
  "subtasks": [
    {
      "id": "weak",
      "title": "Weak leaf",
      "description": "Do something",
      "type": "LEAF",
      "contract": {"outputs": []},
      "dod": {"commands": ["ls"]}
    },
    {
      "id": "x",
      "title": "Writer x",
      "description": "Write shared.go",
      "type": "LEAF",
      "contract": {"outputs": ["shared.go"]},
      "dod": {"commands": ["test -f shared.go"]}
    },
    {
      "id": "y",
      "title": "Writer y",
      "description": "Also write shared.go",
      "type": "LEAF",
      "contract": {"outputs": ["shared.go"]},
      "dod": {"commands": ["test -f shared.go"]}
    }
  ]
}`

func TestOrchestrator_PlanWarnings(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: weakPlan}, nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_plan_warnings"
	cfg.Goal = "plan quality check"
	cfg.Stream = false

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunPlan(context.Background()); err != nil {
		t.Fatal(err)
	}

	warns := o.planWarnings()
	joined := strings.Join(warns, "\n")
	if len(warns) != 3 {
		t.Fatalf("expected 3 warnings, got %d:\n%s", len(warns), joined)
	}
	for _, want := range []string{
		"weak (Weak leaf): verification is only a placeholder (ls)",
		"weak (Weak leaf): no declared outputs",
		"x and y (siblings) both declare output shared.go",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing warning %q in:\n%s", want, joined)
		}
	}
}

func TestOrchestrator_UsageAccumulatesAcrossResume(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	usage := func(h func(context.Context, llm.Request) (*llm.Response, error)) func(context.Context, llm.Request) (*llm.Response, error) {
		return func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			resp, err := h(ctx, req)
			if resp != nil {
				resp.Usage = llm.Usage{PromptTokens: 7, CompletionTokens: 3}
			}
			return resp, err
		}
	}
	architect := func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: twoLeafPlan}, nil
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_usage_resume"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.MaxRetries = 1

	// Phase 1: plan only — one decompose call recorded on the root node.
	planClient := &llm.ScriptedClient{Handle: usage(architect)}
	o, err := NewFromGoal(cfg, planClient, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	root, _ := o.tree.CloneNode("root")
	if root.TokenUsage.Calls != 1 || root.TokenUsage.TotalTokens != 10 {
		t.Fatalf("root usage after plan: %+v", root.TokenUsage)
	}

	// Phase 2: resume and execute — four worker calls, root usage untouched.
	o2, err := Load(cfg, &llm.ScriptedClient{Handle: usage(twoLeafWorkerHandle(nil))}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta"} {
		n, _ := o2.tree.CloneNode(id)
		if n.TokenUsage.Calls != 2 || n.TokenUsage.TotalTokens != 20 {
			t.Fatalf("%s usage: %+v", id, n.TokenUsage)
		}
	}
	root, _ = o2.tree.CloneNode("root")
	if root.TokenUsage.Calls != 1 {
		t.Fatalf("root usage changed after resume: %+v", root.TokenUsage)
	}
	total := o2.tree.TotalTokenUsage()
	if total.Calls != 5 || total.PromptTokens != 35 || total.CompletionTokens != 15 || total.TotalTokens != 50 {
		t.Fatalf("session total: %+v", total)
	}

	// Session reload keeps the accumulated numbers.
	o3, err := Load(cfg, &llm.ScriptedClient{Handle: usage(twoLeafWorkerHandle(nil))}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got := o3.tree.TotalTokenUsage(); got.Calls != 5 || got.TotalTokens != 50 {
		t.Fatalf("usage lost on reload: %+v", got)
	}
	if n, ok := o3.tree.CloneNode("alpha"); !ok || n.State != models.TaskStateCompleted {
		t.Fatalf("alpha state after reload: ok=%v node=%+v", ok, n)
	}
}
