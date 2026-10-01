package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAIClient talks to any OpenAI-compatible /chat/completions endpoint.
type OpenAIClient struct {
	APIKey      string
	BaseURL     string
	HTTPClient  *http.Client
	MaxRetries  int           // 0 = retry retryable errors forever
	MaxBackoff  time.Duration // cap for exponential backoff; default 30s
	MinInterval time.Duration
	ExtraJSON   string
	OnToken     func(string)

	mu      sync.Mutex
	lastReq time.Time
}

// NewOpenAIClient constructs a client. baseURL should look like https://api.openai.com/v1
func NewOpenAIClient(apiKey, baseURL string, timeout time.Duration) *OpenAIClient {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIClient{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		MaxRetries: 0,
		MaxBackoff: 30 * time.Second,
		HTTPClient: &http.Client{Timeout: timeout + 15*time.Second},
	}
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// StreamOptions asks for usage in the final SSE chunk (OpenAI-compatible
	// providers ignore or honor it; a rejection falls back to non-streaming).
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type apiResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []ToolCall      `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage Usage           `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// Chat sends a chat completion request. Streaming is used when OnToken is set
// or when req.Stream is true; otherwise a single JSON response is fetched.
func (c *OpenAIClient) Chat(ctx context.Context, req Request) (*Response, error) {
	c.throttle()

	if _, ok := ctx.Deadline(); !ok && c.HTTPClient != nil && c.HTTPClient.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.HTTPClient.Timeout)
		defer cancel()
	}

	useStream := req.Stream || c.OnToken != nil
	var lastErr error

	for attempt := 0; ; attempt++ {
		// Never start or retry a request once the caller gave up.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.MaxRetries > 0 && attempt >= c.MaxRetries {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("LLM request failed")
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt, c.MaxBackoff)):
			}
		}

		stream := useStream && attempt == 0
		resp, err := c.do(ctx, req, stream)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// A canceled/deadline context is terminal even if the transport
		// reported it as a retryable network error.
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if !retryable(err) {
			if stream {
				useStream = false
				continue
			}
			return nil, err
		}
	}
}

func (c *OpenAIClient) do(ctx context.Context, req Request, stream bool) (*Response, error) {
	payload := apiRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if stream {
		payload.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body, err = mergeExtraJSON(body, c.ExtraJSON)
	if err != nil {
		return nil, err
	}

	endpoint := c.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, &httpError{status: 0, msg: err.Error(), retry: true}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusUnauthorized {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		return nil, &httpError{status: httpResp.StatusCode, msg: string(raw), retry: false}
	}
	if httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		return nil, &httpError{status: httpResp.StatusCode, msg: string(raw), retry: true}
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, &httpError{status: httpResp.StatusCode, msg: string(raw), retry: false}
	}

	if stream {
		return c.readStream(httpResp.Body)
	}

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var parsed apiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w: %s", err, truncate(string(raw), 300))
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in LLM response")
	}
	ch := parsed.Choices[0]
	return &Response{
		Content:   decodeContent(ch.Message.Content),
		ToolCalls: ch.Message.ToolCalls,
		Usage:     parsed.Usage,
		Finish:    ch.FinishReason,
	}, nil
}

func (c *OpenAIClient) readStream(r io.Reader) (*Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var content strings.Builder
	toolCalls := map[int]*ToolCall{}
	finish := ""
	usage := Usage{}

	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk apiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage.TotalTokens > 0 {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		deltaText := decodeContent(ch.Delta.Content)
		if deltaText != "" {
			content.WriteString(deltaText)
			if c.OnToken != nil {
				c.OnToken(deltaText)
			}
		}
		// Some providers send the full message in stream mode.
		if full := decodeContent(ch.Message.Content); full != "" && content.Len() == 0 {
			content.WriteString(full)
		}
		if len(ch.Message.ToolCalls) > 0 && len(toolCalls) == 0 {
			for i, tc := range ch.Message.ToolCalls {
				cp := tc
				toolCalls[i] = &cp
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			cur, ok := toolCalls[tc.Index]
			if !ok {
				cur = &ToolCall{ID: tc.ID, Type: tc.Type}
				if cur.Type == "" {
					cur.Type = "function"
				}
				toolCalls[tc.Index] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := &Response{
		Content: content.String(),
		Finish:  finish,
		Usage:   usage,
	}
	if len(toolCalls) > 0 {
		// Preserve index order.
		max := -1
		for i := range toolCalls {
			if i > max {
				max = i
			}
		}
		for i := 0; i <= max; i++ {
			if tc, ok := toolCalls[i]; ok {
				out.ToolCalls = append(out.ToolCalls, *tc)
			}
		}
	}
	return out, nil
}

func (c *OpenAIClient) throttle() {
	if c.MinInterval <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := c.MinInterval - time.Since(c.lastReq)
	if wait > 0 {
		time.Sleep(wait)
	}
	c.lastReq = time.Now()
}

func decodeContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return strings.TrimSpace(string(raw))
}

func mergeExtraJSON(body []byte, extra string) ([]byte, error) {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return body, nil
	}
	var base map[string]any
	if err := json.Unmarshal(body, &base); err != nil {
		return nil, err
	}
	var add map[string]any
	if err := json.Unmarshal([]byte(extra), &add); err != nil {
		return nil, fmt.Errorf("invalid -extra JSON: %w", err)
	}
	for k, v := range add {
		base[k] = v
	}
	return json.Marshal(base)
}

type httpError struct {
	status int
	msg    string
	retry  bool
}

func (e *httpError) Error() string {
	if e.status == 0 {
		return e.msg
	}
	return fmt.Sprintf("LLM HTTP %d: %s", e.status, strings.TrimSpace(e.msg))
}

func retryable(err error) bool {
	he, ok := err.(*httpError)
	return ok && he.retry
}

func backoff(failCount int, cap time.Duration) time.Duration {
	min := time.Second
	if cap <= 0 {
		cap = 30 * time.Second
	}
	d := min
	for i := 1; i < failCount; i++ {
		if d > cap/2 {
			return cap
		}
		d *= 2
	}
	if d > cap {
		return cap
	}
	return d
}
