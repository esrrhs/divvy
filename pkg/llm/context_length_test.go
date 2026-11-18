package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextLengthExceededClassification(t *testing.T) {
	matched := []struct {
		status int
		msg    string
	}{
		{400, `{"error":{"message":"Maximum context length exceeded.","code":"context_length_exceeded"}}`},
		{422, `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`},
		{400, "maximum context length exceeded"},
		{400, "This model's maximum context length is 8192 tokens"},
		{422, "prompt is too long"},
		{400, `{"error":{"message":"Please reduce the length of the messages.","code":"prompt_too_long"}}`},
		// Structured context code wins over incidental tool wording.
		{400, `{"error":{"message":"This model does not support tools.","code":"context_length_exceeded"}}`},
	}
	for _, tc := range matched {
		err := &httpError{status: tc.status, msg: tc.msg, retry: false}
		if !ContextLengthExceeded(err) {
			t.Errorf("status %d %q should be context-length exceeded", tc.status, tc.msg)
		}
		if !ContextLengthExceeded(fmt.Errorf("provider: %w", err)) {
			t.Errorf("wrapped status %d should still be context-length exceeded", tc.status)
		}
		if ToolsUnsupported(err) && ContextLengthExceeded(err) {
			t.Errorf("status %d %q must not be both context-length and tools-unsupported", tc.status, tc.msg)
		}
	}

	rejected := []error{
		nil,
		errors.New("maximum context length exceeded"),
		context.Canceled,
		io.ErrUnexpectedEOF,
		&httpError{status: 401, msg: "maximum context length exceeded", retry: false},
		&httpError{status: 403, msg: "maximum context length exceeded", retry: false},
		&httpError{status: 429, msg: "maximum context length exceeded", retry: true},
		&httpError{status: 500, msg: "maximum context length exceeded", retry: true},
		&httpError{status: 0, msg: "connection reset: maximum context length exceeded", retry: true},
		&httpError{status: 400, msg: `{"error":{"message":"This model does not support tools.","param":"tools"}}`, retry: false},
		&httpError{status: 400, msg: `{"error":{"message":"Unknown parameter: tools","param":"tools","code":"unknown_parameter"}}`, retry: false},
		&httpError{status: 400, msg: "invalid api key", retry: false},
		&httpError{status: 400, msg: "model not found", retry: false},
		&httpError{status: 400, msg: "bad request", retry: false},
	}
	for i, err := range rejected {
		if ContextLengthExceeded(err) {
			t.Errorf("case %d %v must not be treated as context-length exceeded", i, err)
		}
	}
}

func TestContextLengthExceededFromRealClient(t *testing.T) {
	const body = `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	c.MaxRetries = 1
	_, err := c.Chat(context.Background(), Request{
		Model:    "scripted",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected 400")
	}
	if !ContextLengthExceeded(err) {
		t.Fatalf("real client 400 context rejection was not classified: %v", err)
	}
	if ToolsUnsupported(err) {
		t.Fatalf("context-length error was also classified as tools-unsupported: %v", err)
	}
	if strings.Contains(err.Error(), "sk-scripted") {
		t.Errorf("error leaked the API key: %v", err)
	}
}
