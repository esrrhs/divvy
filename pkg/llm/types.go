package llm

import "context"

// Role constants for chat messages.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one turn in a chat completion request.
type Message struct {
	Role string `json:"role"`
	// Content is always serialized, even when empty. With `omitempty` an
	// empty reply (a model that answered with nothing) made the field
	// disappear, and the endpoint then read the message as "content: null"
	// and rejected the whole request — one silent model reply killed the
	// turn with a 400 instead of the engine's own "invalid reply" handling.
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is an OpenAI-style function call emitted by the model.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}

// ToolFunction holds the name and JSON-string arguments of a tool call.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is an OpenAI-style tool definition.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Request is a chat completion request.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// OnToken, if set, receives each incremental text delta as the
	// provider streams it. It is never serialized to the HTTP payload.
	OnToken func(string) `json:"-"`
}

// Usage reports token usage when the provider returns it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is a normalized chat completion result.
type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
	Finish    string
}

// Client is the LLM abstraction used by decomposer and worker.
type Client interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}
