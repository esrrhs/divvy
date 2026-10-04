package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient builds a client pointed at srv with retries capped so tests
// exercising the retry loop finish quickly.
func newTestClient(srv *httptest.Server, mutate func(*OpenAIClient)) *OpenAIClient {
	c := NewOpenAIClient("test-key", srv.URL, 5*time.Second)
	c.MaxBackoff = 10 * time.Millisecond
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestChatNonStreamDecodesToolCallsAndFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body: %v", err)
		}
		// The stream field is omitempty, so a non-stream request omits it
		// entirely rather than sending stream:false.
		if v, present := got["stream"]; present && v != false {
			t.Errorf("non-stream request should not enable streaming, got %v", v)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q", auth)
		}
		w.Write([]byte(`{
			"choices":[{"message":{"content":"hi","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool name = %q", resp.ToolCalls[0].Function.Name)
	}
	if resp.Finish != "tool_calls" {
		t.Errorf("finish = %q", resp.Finish)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestChatRetriesOnServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("upstream boom"))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("should have recovered: %v", err)
	}
	if resp.Content != "recovered" {
		t.Fatalf("content = %q", resp.Content)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server saw %d calls, want 3", n)
	}
}

func TestChatRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("slow down"))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// TestChatDoesNotRetryOn4xx guards against burning quota on a request the
// server will never accept (bad model name, malformed payload).
func TestChatDoesNotRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"unknown model"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "nope",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected an error for 400")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("400 must not be retried, saw %d calls", n)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention the status, got %v", err)
	}
}

func TestChatUnauthorizedIsTerminal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected an error for 401")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("401 must not be retried, saw %d calls", n)
	}
}

func TestChatRespectsMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("still down"))
	}))
	defer srv.Close()

	_, err := newTestClient(srv, func(c *OpenAIClient) { c.MaxRetries = 2 }).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected an error once retries were exhausted")
	}
	if n := calls.Load(); n > 3 {
		t.Errorf("MaxRetries=2 should stop early, saw %d calls", n)
	}
}

// TestChatFallsBackToNonStream covers the provider that rejects SSE: the first
// streaming attempt fails, and the client must retry without streaming rather
// than surface an error.
func TestChatFallsBackToNonStream(t *testing.T) {
	var sawStream atomic.Bool
	var firstStream atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] == true {
			sawStream.Store(true)
			if firstStream.CompareAndSwap(false, true) {
				// 400 is terminal for a stream request, which triggers the
				// non-stream fallback path.
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("streaming unsupported"))
				return
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"non-stream ok"}}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "non-stream ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if !sawStream.Load() {
		t.Error("expected the first attempt to use streaming")
	}
}

func TestChatRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newTestClient(srv, nil).Chat(ctx, Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected a context error")
	}
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("err = %v, want an empty-choices error", err)
	}
}

func TestChatSurfacesDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

// TestReadStreamAccumulatesToolCallFragments covers providers that stream a
// tool call's name and arguments across several chunks, keyed by index.
func TestReadStreamAccumulatesToolCallFragments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"pa\"}}]}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\"a.go\\\"}\"}}]}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Errorf("id/name = %q/%q", tc.ID, tc.Function.Name)
	}
	if tc.Function.Arguments != `{"path":"a.go"}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
	if resp.Finish != "tool_calls" {
		t.Errorf("finish = %q", resp.Finish)
	}
}

// TestReadStreamPreservesToolCallIndexOrder verifies multi-tool streams keep
// index order rather than map iteration order.
func TestReadStreamPreservesToolCallIndexOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Emit index 1 before index 0 on purpose.
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"second\",\"function\":{\"name\":\"b\",\"arguments\":\"{}\"}}]}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"first\",\"function\":{\"name\":\"a\",\"arguments\":\"{}\"}}]}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "first" || resp.ToolCalls[1].ID != "second" {
		t.Errorf("order = %q,%q want first,second", resp.ToolCalls[0].ID, resp.ToolCalls[1].ID)
	}
}

// TestReadStreamIgnoresNoise covers SSE comments, blank lines, and malformed
// chunks, all of which appear in real provider output.
func TestReadStreamIgnoresNoise(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(": keep-alive comment\n"))
		w.Write([]byte("\n"))
		w.Write([]byte("data: {broken json\n"))
		w.Write([]byte("data: {\"choices\":[]}\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"kept\"}}]}\n"))
		w.Write([]byte("data: [DONE]\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"after done\"}}]}\n"))
	}))
	defer srv.Close()

	resp, err := newTestClient(srv, nil).Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "kept" {
		t.Fatalf("content = %q; data after [DONE] must be ignored", resp.Content)
	}
}

func TestOnTokenReceivesStreamedText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var got strings.Builder
	c := newTestClient(srv, nil)
	c.OnToken = func(s string) { got.WriteString(s) }
	// OnToken alone switches the client into streaming mode.
	resp, err := c.Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "ab" {
		t.Errorf("OnToken received %q, want %q", got.String(), "ab")
	}
	if resp.Content != "ab" {
		t.Errorf("content = %q", resp.Content)
	}
}

// TestMergeExtraJSON covers the -extra passthrough used to toggle thinking
// and other provider-specific request fields.
func TestMergeExtraJSON(t *testing.T) {
	base := []byte(`{"model":"m","temperature":0.2}`)

	got, err := mergeExtraJSON(base, "")
	if err != nil {
		t.Fatalf("empty extra should be a no-op: %v", err)
	}
	if string(got) != string(base) {
		t.Errorf("empty extra changed the body: %s", got)
	}

	got, err = mergeExtraJSON(base, `{"enable_thinking":false,"top_p":0.8}`)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", m["enable_thinking"])
	}
	if m["top_p"] != 0.8 {
		t.Errorf("top_p = %v, want 0.8", m["top_p"])
	}
	// Existing keys must survive the merge.
	if m["model"] != "m" || m["temperature"] != 0.2 {
		t.Errorf("merge clobbered existing keys: %s", got)
	}

	// Extra must win on conflict: that is the point of the flag.
	got, err = mergeExtraJSON(base, `{"temperature":0.9}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["temperature"] != 0.9 {
		t.Errorf("temperature = %v, want the extra value 0.9", m["temperature"])
	}

	if _, err := mergeExtraJSON(base, `{not json`); err == nil {
		t.Error("invalid -extra JSON should be rejected")
	} else if !strings.Contains(err.Error(), "-extra") {
		t.Errorf("error should name the flag, got %v", err)
	}
}

func TestChatAppliesExtraJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["enable_thinking"] != false {
			t.Errorf("extra not merged into the request: %v", got)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, nil)
	c.ExtraJSON = `{"enable_thinking":false}`
	if _, err := c.Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorsAreClassified(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantMsg     string
		wantRetryab bool
	}{
		{"network error has no status", &httpError{status: 0, msg: "dial tcp: refused", retry: true}, "dial tcp: refused", true},
		{"429 is retryable", &httpError{status: 429, msg: "slow down", retry: true}, "LLM HTTP 429", true},
		{"500 is retryable", &httpError{status: 500, msg: "boom", retry: true}, "LLM HTTP 500", true},
		{"400 is terminal", &httpError{status: 400, msg: "bad", retry: false}, "LLM HTTP 400", false},
	}
	for _, tc := range cases {
		if !strings.Contains(tc.err.Error(), tc.wantMsg) {
			t.Errorf("%s: message = %q, want it to contain %q", tc.name, tc.err.Error(), tc.wantMsg)
		}
		if got := retryable(tc.err); got != tc.wantRetryab {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.wantRetryab)
		}
	}
	// A non-httpError must never be retried.
	if retryable(context.Canceled) {
		t.Error("a plain context error must not be classified retryable")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	cap := 30 * time.Second
	if got := backoff(1, cap); got != time.Second {
		t.Errorf("backoff(1) = %s, want 1s", got)
	}
	if got := backoff(3, cap); got != 4*time.Second {
		t.Errorf("backoff(3) = %s, want 4s", got)
	}
	// Must saturate at the cap rather than overflow.
	for _, n := range []int{6, 10, 100} {
		if got := backoff(n, cap); got != cap {
			t.Errorf("backoff(%d) = %s, want the cap %s", n, got, cap)
		}
	}
	// A non-positive cap falls back to the 30s default.
	if got := backoff(10, 0); got != 30*time.Second {
		t.Errorf("backoff with no cap = %s, want 30s", got)
	}
}

// TestThrottleSpacesRequests verifies MinInterval is honoured across calls.
func TestThrottleSpacesRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, func(c *OpenAIClient) { c.MinInterval = 40 * time.Millisecond })
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Chat(context.Background(), Request{
			Model:    "m",
			Messages: []Message{{Role: RoleUser, Content: "x"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	// 3 calls at 40ms apart means at least 2 gaps.
	if elapsed < 60*time.Millisecond {
		t.Errorf("3 calls took %s; MinInterval=40ms was not honoured", elapsed)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// TestDecodeContentHandlesShapeVariants covers providers that return content
// as a plain string, a null, or a content-parts array.
func TestDecodeContentHandlesShapeVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"hello"`, "hello"},
		{"null", `null`, ""},
		{"empty", ``, ""},
		{"parts array", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"bare number", `42`, "42"},
	}
	for _, tc := range cases {
		if got := decodeContent(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: decodeContent(%s) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestTruncateCapsLongText(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncate(long, 100)
	// truncate appends an ellipsis after the cut, so the result is n+3 bytes.
	if len(got) != 103 {
		t.Errorf("len = %d, want 103 (100 + ellipsis)", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated text should end with an ellipsis, got %q", got)
	}
	if got := truncate("short", 100); got != "short" {
		t.Errorf("short input was altered: %q", got)
	}
}

func TestIsIdent(t *testing.T) {
	cases := map[string]bool{
		"json": true, "go": true, "c++": true, "c#": false, "": false, "a b": false, "语言": true,
	}
	for in, want := range cases {
		if got := isIdent(in); got != want {
			t.Errorf("isIdent(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestStripFenceUnwrapsMarkdownCodeBlocks is the single most common weak-model
// failure mode: wrapping the JSON action in a ``` fence.
func TestStripFenceUnwrapsMarkdownCodeBlocks(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bare json", `{"a":1}`, `{"a":1}`},
		{"fenced no lang", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced json lang", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced with spaces", "  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
		{"not a fence", "text ``` more", "text ``` more"},
	}
	for _, tc := range cases {
		if got := stripFence(tc.in); got != tc.want {
			t.Errorf("%s: stripFence = %q, want %q", tc.name, got, tc.want)
		}
	}
}
