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

func TestToolsUnsupportedClassification(t *testing.T) {
	supported := []struct {
		status int
		msg    string
	}{
		{400, `{"error":{"message":"This model does not support tools."}}`},
		{400, "this model doesn't support tool use"},
		{422, "function calling is not supported for this model"},
		{400, "Unknown parameter: tools"},
		{400, "unrecognized request argument supplied: tools"},
		{400, "'tools' is not a valid parameter"},
		{400, "tool_choice is not supported"},
		{400, `{"error":{"message":"Unknown parameter: tools","param":"tools","code":"unknown_parameter"}}`},
		{422, `{"error":{"message":"Unsupported parameter","param":"tool_choice","code":"unsupported_parameter"}}`},
		{400, "this model doesn’t support tool use"},
	}
	for _, tc := range supported {
		err := &httpError{status: tc.status, msg: tc.msg, retry: false}
		if !ToolsUnsupported(err) {
			t.Errorf("status %d %q should be tools-unsupported", tc.status, tc.msg)
		}
		if !ToolsUnsupported(fmt.Errorf("provider: %w", err)) {
			t.Errorf("wrapped status %d should still be tools-unsupported", tc.status)
		}
	}

	rejected := []error{
		nil,
		errors.New("this model does not support tools"),
		context.Canceled,
		io.ErrUnexpectedEOF,
		&httpError{status: 401, msg: "this model does not support tools", retry: false},
		&httpError{status: 403, msg: "this model does not support tools", retry: false},
		&httpError{status: 429, msg: "this model does not support tools", retry: true},
		&httpError{status: 500, msg: "this model does not support tools", retry: true},
		&httpError{status: 0, msg: "connection reset: this model does not support tools", retry: true},
		&httpError{status: 400, msg: "maximum context length exceeded", retry: false},
		&httpError{status: 400, msg: "invalid tool_call id in the request", retry: false},
		&httpError{status: 400, msg: "model not found", retry: false},
		&httpError{status: 404, msg: "this model does not support tools", retry: false},
		// P2/F1: context overflow that affirms tool support must not be a capability rejection.
		&httpError{status: 400, msg: `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`, retry: false},
		&httpError{status: 422, msg: `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`, retry: false},
		&httpError{status: 400, msg: "Maximum context length exceeded. This model does support tool use.", retry: false},
		&httpError{status: 400, msg: `{"error":{"message":"This model does support tool use."}}`, retry: false},
		&httpError{status: 400, msg: `{"error":{"message":"Invalid value","param":"temperature","code":"invalid_value"}}`, retry: false},
		&httpError{status: 400, msg: `{"error":{"message":"bad request"}}`, retry: false},
		&httpError{status: 400, msg: `{"error":{"message":"invalid tools payload"}}`, retry: false},
		&httpError{status: 400, msg: "tools", retry: false},
		// Known unrelated structured code wins over incidental tool wording.
		&httpError{status: 400, msg: `{"error":{"message":"This model does not support tools.","code":"context_length_exceeded"}}`, retry: false},
	}
	for i, err := range rejected {
		if ToolsUnsupported(err) {
			t.Errorf("case %d %v must not be treated as tools-unsupported", i, err)
		}
	}
}

func TestToolsUnsupportedFromRealClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "sk-") && strings.Contains(string(mustRead(r)), "sk-real") {
			t.Error("request unexpectedly contained a real-looking secret in the body")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model does not support tools.","param":"tools"}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	c.MaxRetries = 1
	_, err := c.Chat(context.Background(), Request{Model: "scripted", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: []Tool{{Type: "function"}}})
	if err == nil {
		t.Fatal("expected 400")
	}
	if !ToolsUnsupported(err) {
		t.Fatalf("real client 400 tools rejection was not classified: %v", err)
	}
	if strings.Contains(err.Error(), "sk-scripted") {
		t.Errorf("error leaked the API key: %v", err)
	}
}

func mustRead(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

func TestToolsUnsupportedContextLengthAffirmingFromRealClient(t *testing.T) {
	const body = `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
			c.MaxRetries = 1
			_, err := c.Chat(context.Background(), Request{
				Model:    "scripted",
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
				Tools:    []Tool{{Type: "function"}},
			})
			if err == nil {
				t.Fatal("expected context-length HTTP error")
			}
			if ToolsUnsupported(err) {
				t.Fatalf("context-length error affirming tool support was misclassified: %v", err)
			}
			if strings.Contains(err.Error(), "sk-scripted") {
				t.Errorf("error leaked the API key: %v", err)
			}
		})
	}
}
