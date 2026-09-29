package llm

import (
	"context"
	"sync"
)

// ScriptedClient returns canned responses in order. Used by tests.
// Safe for concurrent use (parallel leaf execution calls Chat concurrently).
type ScriptedClient struct {
	Handle func(ctx context.Context, req Request) (*Response, error)

	mu       sync.Mutex
	Requests []Request
}

func (c *ScriptedClient) Chat(ctx context.Context, req Request) (*Response, error) {
	c.mu.Lock()
	c.Requests = append(c.Requests, req)
	c.mu.Unlock()
	if c.Handle == nil {
		return nil, nil
	}
	return c.Handle(ctx, req)
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
