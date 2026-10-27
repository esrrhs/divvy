package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewOpenAIClientDefaults covers the default timeout and endpoint.
func TestNewOpenAIClientDefaults(t *testing.T) {
	c := NewOpenAIClient("key", "", 0)
	if c.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("BaseURL = %q", c.BaseURL)
	}
	if c.HTTPClient.Timeout != 120*time.Second+15*time.Second {
		t.Fatalf("Timeout = %s", c.HTTPClient.Timeout)
	}
}

// TestChatDeadlineExpiresDuringBackoff covers the select branch where the
// caller's deadline lapses while the client waits to retry: the deadline must
// win over the backoff timer and surface as the error.
func TestChatDeadlineExpiresDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("down"))
	}))
	defer srv.Close()

	c := NewOpenAIClient("k", srv.URL, 5*time.Second) // default 30s backoff cap
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Chat(ctx, Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestChatCanceledWhileRequestInFlight covers the post-do check: a context
// canceled during a stuck request is terminal even though the transport
// classifies the failure as retryable.
func TestChatCanceledWhileRequestInFlight(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Drain the request body so the server notices the client's abort
		// (an unread body masks the connection close) and unwinds.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewOpenAIClient("k", srv.URL, 5*time.Second)
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
		errCh <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after cancellation")
	}
}

// TestChatExtraJSONInvalid covers the request-build failure when -extra is
// not valid JSON.
func TestChatExtraJSONInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	c := NewOpenAIClient("k", srv.URL, 5*time.Second)
	c.ExtraJSON = "{not json"
	_, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "-extra") {
		t.Fatalf("err = %v, want an -extra error", err)
	}
}

// TestChatInvalidEndpointURL covers http.NewRequest failing on a malformed
// base URL.
func TestChatInvalidEndpointURL(t *testing.T) {
	c := NewOpenAIClient("k", "http://192.168.0.%zz", time.Second)
	_, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("expected a URL parse error")
	}
}

// TestChatConnectionRefused covers the transport-level failure branch.
func TestChatConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // port now refuses connections

	c := NewOpenAIClient("k", srv.URL, time.Second)
	c.MaxBackoff = 10 * time.Millisecond
	c.MaxRetries = 2
	_, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("expected a connection error")
	}
}

// errBody is an io.ReadCloser whose reads fail after the buffered prefix.
type errBody struct {
	r io.Reader
}

func (e *errBody) Read(p []byte) (int, error) { return e.r.Read(p) }
func (e *errBody) Close() error               { return nil }

// failingTransport serves one response whose body read fails.
type failingTransport struct {
	body io.Reader
}

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &errBody{r: f.body},
	}, nil
}

// boomReader always fails; it stands in for a transport that dies mid-body.
type boomReader struct{}

func (boomReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestChatNonStreamBodyReadError covers io.ReadAll failing on a non-stream
// response.
func TestChatNonStreamBodyReadError(t *testing.T) {
	c := NewOpenAIClient("k", "http://unit.test", time.Second)
	c.HTTPClient = &http.Client{Transport: failingTransport{
		body: io.MultiReader(strings.NewReader(`{"choices":[{"message":{"content":"hi"}}]}`), boomReader{}),
	}}
	_, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("expected a body read error")
	}
}

// TestReadStreamBodyError covers the scanner error path mid-SSE.
func TestReadStreamBodyError(t *testing.T) {
	c := NewOpenAIClient("k", "http://unit.test", time.Second)
	c.HTTPClient = &http.Client{Transport: failingTransport{
		body: io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"), boomReader{}),
	}}
	_, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}, Stream: true})
	if err == nil {
		t.Fatal("expected a stream read error")
	}
}

// TestReadStreamSkipsNonDataLines covers SSE fields like "event:" that are
// neither comments nor data.
func TestReadStreamSkipsNonDataLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := NewOpenAIClient("k", srv.URL, 5*time.Second).Chat(context.Background(),
		Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// TestReadStreamFullMessageAndToolCalls covers providers that stream the
// complete message (content + tool calls) instead of deltas.
func TestReadStreamFullMessageAndToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"message":{"content":"full text","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resp, err := NewOpenAIClient("k", srv.URL, 5*time.Second).Chat(context.Background(),
		Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "full text" {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c1" || resp.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

// --- parser edges ------------------------------------------------------------

func TestExtractJSONEmptyInput(t *testing.T) {
	if _, err := ExtractJSON("   "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want the empty-output error", err)
	}
}

// TestExtractJSONLeadingFence covers the fence strip performed inside
// ExtractJSON itself (no prose before the fence).
func TestExtractJSONLeadingFence(t *testing.T) {
	got, err := ExtractJSON("```json\n{\"a\":1}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"a"`) {
		t.Fatalf("json = %s", got)
	}
}

// TestExtractJSONUnopenable covers output whose first brace can never balance
// even after truncation repair, plus a stray bracket that truncation cannot
// rescue either.
func TestExtractJSONUnopenable(t *testing.T) {
	if _, err := ExtractJSON("]"); err == nil {
		t.Fatal("a lone closing bracket must fail")
	}
	if _, err := ExtractJSON("{]"); err == nil {
		t.Fatal("a mismatched brace/bracket must fail")
	}
	if _, err := ExtractJSON("[}"); err == nil {
		t.Fatal("a bracket closed by a brace must fail")
	}
	// The tolerant closer balances "]}" against "{", sees nothing left open
	// to repair, and gives up entirely.
	if _, err := ExtractJSON(`{"a": 1]}`); err == nil {
		t.Fatal("a brace closed by a bracket then a brace must fail")
	}
}

// TestExtractJSONCompleteArray covers the array close returning at the
// matching bracket.
func TestExtractJSONCompleteArray(t *testing.T) {
	got, err := ExtractJSON(`prose [1, 2] trailing`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[1, 2]" {
		t.Fatalf("got %q", got)
	}
}

func TestParseActionGarbage(t *testing.T) {
	if _, err := ParseAction("no json here at all"); err == nil {
		t.Fatal("expected an error for non-JSON input")
	}
}

// TestParseActionNestedFunctionWithoutArgs covers the default empty-args map
// when the nested function shape carries no arguments.
func TestParseActionNestedFunctionWithoutArgs(t *testing.T) {
	act, err := ParseAction(`{"function": {"name": "Run"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "run" {
		t.Fatalf("name = %q", act.Name)
	}
	if act.Args == nil {
		t.Fatal("Args must default to an empty map")
	}
	if len(act.Args) != 0 {
		t.Fatalf("args = %v", act.Args)
	}
}

func TestCloseTruncatedJSONEmptyInput(t *testing.T) {
	if got := closeTruncatedJSON(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestCloseTruncatedJSONOpenStringEscape covers the escape handling while the
// scanner is inside an open string, including a trailing backslash that must
// be dropped before the string is closed.
func TestCloseTruncatedJSONOpenStringEscape(t *testing.T) {
	got := closeTruncatedJSON(`{"a": "b\c`)
	if !strings.HasPrefix(got, `{"a"`) {
		t.Fatalf("closeTruncatedJSON mangled the prefix: %q", got)
	}
	if _, err := ExtractJSON(`{"a": "b\c`); err != nil {
		t.Fatalf("truncated string with escape should be repaired: %v", err)
	}
	if _, err := ExtractJSON(`{"a": "b\`); err != nil {
		t.Fatalf("trailing-backslash string should be repaired: %v", err)
	}
}

// TestExtractJSONTruncatedStringValue covers closing a string that ends right
// after a colon, and one inside an array.
func TestExtractJSONTruncatedStringValue(t *testing.T) {
	got, err := ExtractJSON(`{"a": "b`)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := UnmarshalFlexible(got, &m); err != nil {
		t.Fatalf("repaired json %q: %v", got, err)
	}
	if m["a"] != "b" {
		t.Fatalf("a = %v", m["a"])
	}

	arr, err := ExtractJSON(`["x", "y`)
	if err != nil {
		t.Fatal(err)
	}
	var list []any
	if err := UnmarshalFlexible(arr, &list); err != nil {
		t.Fatalf("repaired json %q: %v", arr, err)
	}
	// The incomplete trailing string is dropped by the repair.
	if len(list) != 1 || list[0] != "x" {
		t.Fatalf("list = %v", list)
	}
}

func TestLastNonSpaceAllWhitespace(t *testing.T) {
	if got := lastNonSpace(" \t\n\r "); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

// TestReplayOfUnitHelpers is a compile-time guard for the transport helpers.
func TestTransportHelpersTypes(t *testing.T) {
	var _ http.RoundTripper = failingTransport{}
	var b io.ReadCloser = &errBody{r: strings.NewReader("")}
	if _, err := b.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty errBody read = %v, want io.EOF", err)
	}
}
