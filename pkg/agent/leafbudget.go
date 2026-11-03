package agent

import (
	"errors"
	"fmt"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/models"
)

// ErrLeafBudget marks a leaf that exhausted its per-leaf token/cost ceiling.
// Callers fail fast and signal re-decompose instead of burning the remaining
// session budget on an oversized leaf.
var ErrLeafBudget = errors.New("leaf budget exceeded")

// checkLeafBudget returns ErrLeafBudget when the live node has crossed a
// configured per-leaf ceiling. 0 ceilings are unlimited.
func (o *Orchestrator) checkLeafBudget(nodeID string) error {
	if o.cfg.MaxLeafTokens <= 0 && o.cfg.MaxLeafCost <= 0 {
		return nil
	}
	n, ok := o.tree.CloneNode(nodeID)
	if !ok || n == nil {
		return nil
	}
	if reason := leafBudgetReason(o.cfg, o.pricing, o.cfg.Model, n.TokenUsage); reason != "" {
		return fmt.Errorf("%w: %s — fail fast and re-decompose rather than burning the rest of the session",
			ErrLeafBudget, reason)
	}
	return nil
}

func leafBudgetReason(cfg Config, pricing *cost.Pricing, model string, u models.TokenUsage) string {
	if cfg.MaxLeafTokens > 0 && u.TotalTokens >= cfg.MaxLeafTokens {
		return fmt.Sprintf("token budget: %d tokens used, leaf ceiling %d", u.TotalTokens, cfg.MaxLeafTokens)
	}
	if cfg.MaxLeafCost > 0 && pricing != nil {
		if price, ok := pricing.PriceFor(model); ok {
			c := price.Cost(u.PromptTokens, u.CompletionTokens)
			if c >= cfg.MaxLeafCost {
				return fmt.Sprintf("cost budget: %s used, leaf ceiling %s",
					cost.FormatUSD(c), cost.FormatUSD(cfg.MaxLeafCost))
			}
		}
	}
	return ""
}
