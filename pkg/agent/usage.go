package agent

import (
	"sort"
	"sync"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
)

// UsageTracker accumulates LLM token usage per call kind ("decompose", "worker", ...).
// Safe for concurrent use.
type UsageTracker struct {
	mu     sync.Mutex
	calls  int
	byKind map[string]llm.Usage
}

// NewUsageTracker creates an empty tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{byKind: make(map[string]llm.Usage)}
}

// Add records one LLM call of the given kind. Providers that omit usage fields
// still count toward the call total.
func (t *UsageTracker) Add(kind string, u llm.Usage) {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	k := t.byKind[kind]
	k.PromptTokens += u.PromptTokens
	k.CompletionTokens += u.CompletionTokens
	k.TotalTokens += u.TotalTokens
	t.byKind[kind] = k
}

// Total returns aggregate usage across all kinds and the number of LLM calls.
func (t *UsageTracker) Total() (llm.Usage, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out llm.Usage
	for _, k := range t.byKind {
		out.PromptTokens += k.PromptTokens
		out.CompletionTokens += k.CompletionTokens
		out.TotalTokens += k.TotalTokens
	}
	return out, t.calls
}

// ByKind returns a snapshot of per-kind totals.
func (t *UsageTracker) ByKind() map[string]llm.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]llm.Usage, len(t.byKind))
	for k, v := range t.byKind {
		out[k] = v
	}
	return out
}

// SortedKinds returns per-kind totals ordered by kind name.
func (t *UsageTracker) SortedKinds() ([]string, map[string]llm.Usage) {
	kinds := t.ByKind()
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, kinds
}
