package llm

import (
	"context"
	"sync"
	"time"
)

// ScriptedClient returns canned responses in order. Used by tests.
// Safe for concurrent use (parallel leaf execution calls Chat concurrently).
type ScriptedClient struct {
	Handle func(ctx context.Context, req Request) (*Response, error)

	// TokenParts, when set, are delivered one-by-one to req.OnToken before
	// Chat returns. Used to prove real incremental streaming in tests.
	TokenParts []string
	// StreamChunks splits Handle's Content into this many OnToken deltas
	// when TokenParts is empty and req.OnToken is set (0 or 1 = no split).
	StreamChunks int
	// ChunkDelay waits between synthetic deltas so a test can cancel mid-stream.
	ChunkDelay time.Duration

	mu       sync.Mutex
	Requests []Request
}

func (c *ScriptedClient) Chat(ctx context.Context, req Request) (*Response, error) {
	c.mu.Lock()
	c.Requests = append(c.Requests, req)
	parts := append([]string(nil), c.TokenParts...)
	n := c.StreamChunks
	delay := c.ChunkDelay
	c.mu.Unlock()
	if c.Handle == nil {
		return nil, nil
	}
	resp, err := c.Handle(ctx, req)
	if err != nil || resp == nil || req.OnToken == nil {
		return resp, err
	}
	if len(parts) == 0 && n > 1 && resp.Content != "" {
		parts = splitTokenParts(resp.Content, n)
	}
	for i, p := range parts {
		if err := ctx.Err(); err != nil {
			return resp, err
		}
		if delay > 0 && i > 0 {
			select {
			case <-ctx.Done():
				return resp, ctx.Err()
			case <-time.After(delay):
			}
		}
		req.OnToken(p)
	}
	return resp, err
}

func splitTokenParts(s string, n int) []string {
	if n <= 1 || s == "" {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) < n {
		n = len(runes)
	}
	out := make([]string, 0, n)
	size := (len(runes) + n - 1) / n
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// SequenceClient walks through a list of responses.
type SequenceClient struct {
	Responses []*Response
	Errs      []error

	mu       sync.Mutex
	i        int
	Requests []Request
}

// Index returns how many responses have been served.
func (c *SequenceClient) Index() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.i
}

func (c *SequenceClient) Chat(ctx context.Context, req Request) (*Response, error) {
	c.mu.Lock()
	c.Requests = append(c.Requests, req)
	i := c.i
	c.i++
	c.mu.Unlock()
	if i < len(c.Errs) && c.Errs[i] != nil {
		return nil, c.Errs[i]
	}
	if i >= len(c.Responses) {
		return &Response{Content: `{"thought":"no more scripted responses","action":"finish","args":{"summary":"done"}}`}, nil
	}
	return c.Responses[i], nil
}
