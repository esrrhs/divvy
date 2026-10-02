package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// atomicPlanHandle scripts one atomic leaf: decompose, write a.txt, finish.
// Every call reports prompt=7, completion=3 (10 tokens).
func atomicPlanHandle() func(context.Context, llm.Request) (*llm.Response, error) {
	return func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		usage := llm.Usage{PromptTokens: 7, CompletionTokens: 3}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{
				Content: `{"is_atomic": true, "reason": "single file", "contract": {"outputs": ["a.txt"]}, "dod": {"commands": ["test -f a.txt"]}, "subtasks": []}`,
				Usage:   usage,
			}, nil
		}
		if reqHasToolResult(req) {
			return &llm.Response{
				Content: `{"thought":"done","action":"finish","args":{"summary":"ok"}}`,
				Usage:   usage,
			}, nil
		}
		return &llm.Response{
			Content: `{"thought":"write","action":"write_file","args":{"path":"a.txt","content":"alpha"}}`,
			Usage:   usage,
		}, nil
	}
}

// TestBudgetTokensStopsRun: with a 10-token ceiling the first (decompose)
// call reaches the limit, the run stops immediately and nothing is retried.
func TestBudgetTokensStopsRun(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	client := &llm.ScriptedClient{Handle: atomicPlanHandle()}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_budget_tokens"
	cfg.Goal = "one file"
	cfg.Stream = false
	cfg.BudgetTokens = 10

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(client.Requests) != 1 {
		t.Fatalf("client saw %d requests, want 1 (no retries after trip)", len(client.Requests))
	}
	root := o.tree.GetRoot()
	if root.State != models.TaskStatePending {
		t.Fatalf("root state = %s, want PENDING for clean resume", root.State)
	}
	if _, err := os.Stat(filepath.Join(data, cfg.SessionID+".json")); err != nil {
		t.Fatalf("session not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("leaf file should not exist: run stopped before execution")
	}
}

// TestBudgetCostStopsRun verifies the USD ceiling using the built-in
// gpt-4o-mini price: a decompose call alone already costs ~$0.00000285.
func TestBudgetCostStopsRun(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	client := &llm.ScriptedClient{Handle: atomicPlanHandle()}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_budget_cost"
	cfg.Goal = "one file"
	cfg.Stream = false
	cfg.MaxCost = 0.000001

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(client.Requests) != 1 {
		t.Fatalf("client saw %d requests, want 1", len(client.Requests))
	}
}

// TestBudgetAlreadyExceededOnResume: a resumed session whose recorded usage
// is past the ceiling must stop before making any new LLM call.
func TestBudgetAlreadyExceededOnResume(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	// Complete the goal once with no budget...
	first := &llm.ScriptedClient{Handle: atomicPlanHandle()}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_budget_resume"
	cfg.Goal = "one file"
	cfg.Stream = false
	cfg.MaxRetries = 1
	o, err := NewFromGoal(cfg, first, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// ...then resume under a token ceiling below the recorded spend.
	cfg.BudgetTokens = 5
	second := &llm.ScriptedClient{Handle: atomicPlanHandle()}
	o2, err := Load(cfg, second, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o2.Run(context.Background()); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(second.Requests) != 0 {
		t.Fatalf("resume made %d calls, want 0 (already over budget)", len(second.Requests))
	}
}

func TestLoadPricing(t *testing.T) {
	// Inline JSON overrides a built-in price.
	inline := `{"gpt-4o-mini": {"input": 9.0, "output": 9.0}}`
	p, err := loadPricing(inline)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.PriceFor("gpt-4o-mini")
	if !ok || got.InputPerM != 9.0 {
		t.Fatalf("inline override failed: %+v", got)
	}
	// Untouched built-in entries remain.
	if _, ok := p.PriceFor("gpt-4.1-mini"); !ok {
		t.Fatal("built-in entries lost after merge")
	}

	// File-path form.
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(`{"local-x": {"input": 0.01, "output": 0.02}}`), 0644); err != nil {
		t.Fatal(err)
	}
	p, err = loadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.PriceFor("local-x"); !ok {
		t.Fatal("file-based price missing")
	}

	// Empty spec keeps defaults only.
	if _, err := loadPricing(""); err != nil {
		t.Fatal(err)
	}

	// Missing file and bad JSON are errors.
	if _, err := loadPricing(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing pricing file")
	}
	if _, err := loadPricing(`{bad`); err == nil {
		t.Fatal("expected error for malformed pricing JSON")
	}
}

// TestTreeShowsNodeCost exercises the cost label through the real engine
// rendering hook and pricing implementation together.
func TestTreeShowsNodeCost(t *testing.T) {
	tree := engine.NewTaskTree("sess_cost", "goal", "goal")
	if _, err := tree.AddChild(tree.RootID, "alpha", "Alpha", "do alpha", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode("alpha", func(n *models.TaskNode) error {
		n.TokenUsage = models.TokenUsage{
			Calls: 1, PromptTokens: 1_000_000, CompletionTokens: 2_000_000, TotalTokens: 3_000_000,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	custom, err := cost.ParseJSON(`{"my-model": {"input": 1.0, "output": 3.0}}`)
	if err != nil {
		t.Fatal(err)
	}
	pricing := cost.DefaultPricing().Merge(custom)
	tree.ModelName = "my-model"
	tree.PriceFor = pricing.PriceFor

	out := tree.RenderVisualTree()
	if !strings.Contains(out, "$7.00") {
		t.Fatalf("cost label missing:\n%s", out)
	}

	// Without a known price: no dollar label, token label still present.
	tree.PriceFor = nil
	if out := tree.RenderVisualTree(); strings.Contains(out, "$") {
		t.Fatalf("unexpected cost label:\n%s", out)
	}
}
