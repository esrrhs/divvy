package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/models"
)

// loadPricing builds the effective price table: built-in prices overlaid with
// a custom table given as JSON text or as a path to a JSON file.
func loadPricing(spec string) (*cost.Pricing, error) {
	pricing := cost.DefaultPricing()
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return pricing, nil
	}
	if !strings.HasPrefix(spec, "{") {
		data, err := os.ReadFile(spec)
		if err != nil {
			return nil, fmt.Errorf("read pricing file %s: %w", spec, err)
		}
		spec = string(data)
	}
	custom, err := cost.ParseJSON(spec)
	if err != nil {
		return nil, err
	}
	return pricing.Merge(custom), nil
}

// budgetGuard cancels the run context once a session-wide cost or token
// ceiling is reached. Ceilings count everything persisted on the tree, so a
// resumed session never gets to ignore what was spent earlier.
type budgetGuard struct {
	maxCost   float64
	maxTokens int
	pricing   *cost.Pricing
	model     string
	cancel    context.CancelFunc
	log       *Logger
	spend     func() models.TokenUsage
}

func (g *budgetGuard) active() bool {
	return g.maxCost > 0 || g.maxTokens > 0
}

// check reads current session totals and trips the guard if a ceiling is hit.
func (g *budgetGuard) check() bool {
	u := g.spend()
	reason := ""
	if g.maxTokens > 0 && u.TotalTokens >= g.maxTokens {
		reason = fmt.Sprintf("token budget: %d tokens used, ceiling %d", u.TotalTokens, g.maxTokens)
	}
	if g.maxCost > 0 {
		if price, ok := g.pricing.PriceFor(g.model); ok {
			c := price.Cost(u.PromptTokens, u.CompletionTokens)
			if c >= g.maxCost {
				reason = fmt.Sprintf("cost budget: %s used, ceiling %s",
					cost.FormatUSD(c), cost.FormatUSD(g.maxCost))
			}
		}
	}
	if reason == "" {
		return false
	}
	g.log.Errorf("budget exceeded: %s", reason)
	g.log.Warnf("tree saved. Raise the limit (or pass 0 to disable) and run with -resume")
	g.cancel()
	return true
}

// startBudget wraps ctx with a guard that cancels it when a ceiling is hit.
func (o *Orchestrator) startBudget(ctx context.Context) context.Context {
	g := &budgetGuard{
		maxCost:   o.cfg.MaxCost,
		maxTokens: o.cfg.BudgetTokens,
		pricing:   o.pricing,
		model:     o.cfg.Model,
		log:       o.log,
		spend:     o.tree.TotalTokenUsage,
	}
	if !g.active() {
		return ctx
	}
	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	o.budget = g
	// A resumed session may already be at or beyond its ceiling.
	g.check()
	return ctx
}
