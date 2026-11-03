package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

func TestLeafBudgetReason(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLeafTokens = 1000
	cfg.MaxLeafCost = 0.01
	pricing := cost.DefaultPricing()
	u := models.TokenUsage{PromptTokens: 900, CompletionTokens: 200, TotalTokens: 1100}
	if reason := leafBudgetReason(cfg, pricing, "unknown-model-xyz", u); reason == "" {
		t.Fatal("token ceiling must trip even without a price")
	}
	cfg.MaxLeafTokens = 0
	if reason := leafBudgetReason(cfg, pricing, "unknown-model-xyz", u); reason != "" {
		t.Fatalf("no price => no cost trip, got %q", reason)
	}
}

func TestCheckLeafBudgetError(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "leaf_budget"
	cfg.Goal = "x"
	cfg.MaxLeafTokens = 10
	cfg.Stream = false
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: `{"is_atomic":true,"reason":"x","contract":{"outputs":["x.txt"]},"dod":{"commands":["true"]},"subtasks":[]}`}, nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	_ = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.TokenUsage.TotalTokens = 50
		return nil
	})
	err = o.checkLeafBudget(o.tree.RootID)
	if !errors.Is(err, ErrLeafBudget) {
		t.Fatalf("err=%v", err)
	}
}
