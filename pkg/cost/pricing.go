// Package cost turns token usage into monetary cost and supplies a small
// built-in model price table. Prices are quoted in USD per 1,000,000 tokens.
package cost

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Price is a model's token price in USD per 1,000,000 tokens.
type Price struct {
	InputPerM  float64 `json:"input"`
	OutputPerM float64 `json:"output"`
}

// Zero reports whether no price is known.
func (p Price) Zero() bool { return p.InputPerM == 0 && p.OutputPerM == 0 }

// Cost returns the USD cost of a prompt/completion token pair.
func (p Price) Cost(promptTokens, completionTokens int) float64 {
	return float64(promptTokens)/1_000_000*p.InputPerM +
		float64(completionTokens)/1_000_000*p.OutputPerM
}

// Pricing resolves a model name to a Price. Keys are matched first by exact
// name (case-insensitive), then by the longest case-insensitive substring.
type Pricing struct {
	exact   map[string]Price
	partial []rule // longest pattern first
}

type rule struct {
	pattern string
	price   Price
}

// DefaultPricing ships approximate list prices for common OpenAI models.
// Local or unknown models have no entry; callers treat that as "no estimate".
// Prices can be overridden or extended with a custom JSON table.
func DefaultPricing() *Pricing {
	return mustBuild(map[string]Price{
		"gpt-4o-mini":   {InputPerM: 0.15, OutputPerM: 0.60},
		"gpt-4o":        {InputPerM: 2.50, OutputPerM: 10.00},
		"gpt-4.1-mini":  {InputPerM: 0.40, OutputPerM: 1.60},
		"gpt-4.1":       {InputPerM: 2.00, OutputPerM: 8.00},
		"gpt-4.1-nano":  {InputPerM: 0.10, OutputPerM: 0.40},
		"gpt-3.5-turbo": {InputPerM: 0.50, OutputPerM: 1.50},
		"o3-mini":       {InputPerM: 1.10, OutputPerM: 4.40},
		"o4-mini":       {InputPerM: 1.75, OutputPerM: 7.00},
	})
}

// ParseJSON decodes a price table: {"model name": {"input": 0.15, "output": 0.6}}.
// Values are USD per 1,000,000 tokens. Every key also becomes a substring rule.
func ParseJSON(data string) (*Pricing, error) {
	var table map[string]Price
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &table); err != nil {
		return nil, fmt.Errorf("parse pricing JSON: %w", err)
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("empty pricing table")
	}
	for name, p := range table {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("pricing table contains an empty model name")
		}
		if p.InputPerM < 0 || p.OutputPerM < 0 {
			return nil, fmt.Errorf("negative price for %q", name)
		}
	}
	return build(table), nil
}

// PriceFor resolves a model name to its price.
func (p *Pricing) PriceFor(model string) (Price, bool) {
	if p == nil {
		return Price{}, false
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if price, ok := p.exact[name]; ok {
		return price, true
	}
	for _, r := range p.partial {
		if strings.Contains(name, r.pattern) {
			return r.price, true
		}
	}
	return Price{}, false
}

// Merge overlays other on top of p: custom entries override built-in ones.
func (p *Pricing) Merge(other *Pricing) *Pricing {
	if other == nil {
		return p
	}
	merged := &Pricing{exact: map[string]Price{}}
	for k, v := range p.exact {
		merged.exact[k] = v
	}
	for k, v := range other.exact {
		merged.exact[k] = v
	}
	// Other table's rules take priority on substring matches.
	merged.partial = append(merged.partial, other.partial...)
	merged.partial = append(merged.partial, p.partial...)
	return merged
}

func mustBuild(table map[string]Price) *Pricing {
	p, err := buildFrom(table)
	if err != nil {
		panic(err)
	}
	return p
}

func build(table map[string]Price) *Pricing {
	p, _ := buildFrom(table)
	return p
}

func buildFrom(table map[string]Price) (*Pricing, error) {
	p := &Pricing{exact: make(map[string]Price, len(table))}
	patterns := make([]string, 0, len(table))
	for name, price := range table {
		key := strings.ToLower(strings.TrimSpace(name))
		p.exact[key] = price
		patterns = append(patterns, key)
	}
	// Longest patterns first so the most specific substring wins.
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return patterns[i] < patterns[j]
	})
	for _, key := range patterns {
		p.partial = append(p.partial, rule{pattern: key, price: p.exact[key]})
	}
	return p, nil
}

// FormatUSD renders a dollar amount with precision appropriate to its size.
func FormatUSD(v float64) string {
	switch {
	case v < 0.01:
		return fmt.Sprintf("$%.6f", v)
	case v < 1:
		return fmt.Sprintf("$%.4f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}
