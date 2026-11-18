package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

const toolsUnsupportedBody = `{"error":{"message":"This model does not support tools.","type":"invalid_request_error","param":"tools"}}`

func decodeChatReq(t *testing.T, r *http.Request) llm.Request {
	t.Helper()
	var req llm.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode chat request: %v", err)
	}
	return req
}

func writeChatJSON(w http.ResponseWriter, content string, usage llm.Usage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": usage,
	})
}

func writeChatError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func jsonActContent(action string, args map[string]any) string {
	payload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": action, "args": args})
	return string(payload)
}

// TestDirect_HTTPNativeToolsUnsupportedFallsBackToJSON is the product gap:
// Web/API direct now sends native tool definitions, and a provider that
// clearly rejects the tools field currently aborts the run instead of a
// one-time, consistent JSON-action retry.
func TestDirect_HTTPNativeToolsUnsupportedFallsBackToJSON(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var mu sync.Mutex
	nativeReqs, jsonReqs := 0, 0
	step := 0
	var jsonHadTools, jsonLookedNative, jsonHadToolRole bool
	usage := llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		req := decodeChatReq(t, r)
		mu.Lock()
		defer mu.Unlock()
		if len(req.Tools) > 0 {
			nativeReqs++
			writeChatError(w, http.StatusBadRequest, toolsUnsupportedBody)
			return
		}
		jsonReqs++
		joined := joinReq(req)
		if len(req.Tools) > 0 {
			jsonHadTools = true
		}
		if strings.Contains(joined, "you may call multiple independent tools in one reply") {
			jsonLookedNative = true
		}
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool || len(m.ToolCalls) > 0 {
				jsonHadToolRole = true
			}
		}
		step++
		switch step {
		case 1:
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod}), usage)
		case 2:
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd}), usage)
		case 3:
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest}), usage)
		case 4:
			writeChatJSON(w, jsonActContent(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), usage)
		case 5:
			if !strings.Contains(joined, "FAIL") {
				writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "failed test not returned to the model"}), usage)
				return
			}
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd}), usage)
		case 6:
			writeChatJSON(w, jsonActContent(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), usage)
		default:
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{
				"summary": "The first go test failed because Add used subtraction; I changed it to addition and go test passed.",
			}), usage)
		}
	}))
	t.Cleanup(srv.Close)

	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	client.MaxRetries = 1
	cfg := directCfg(t, work, data, "Implement Add(a, b int) int in add.go with a test; go test must pass.")
	cfg.NativeTools = true
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("fallback run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if nativeReqs != 1 {
		t.Fatalf("native tools should be tried once then remembered, got %d native requests", nativeReqs)
	}
	if jsonReqs < 6 {
		t.Fatalf("expected a JSON repair loop after fallback, got %d JSON requests", jsonReqs)
	}
	if jsonHadTools || jsonLookedNative || jsonHadToolRole {
		t.Fatalf("JSON retry was not a consistent downgrade (tools=%v native-guide=%v tool-role=%v)", jsonHadTools, jsonLookedNative, jsonHadToolRole)
	}
	if strings.Contains(strings.ToLower(ctl.summary), "failed test not returned") {
		t.Fatalf("repair failed: %q", ctl.summary)
	}
	if !strings.Contains(ctl.summary, "subtraction") {
		t.Fatalf("expected post-repair summary, got %q", ctl.summary)
	}
	if out, err := runGoTest(t, work); err != nil {
		t.Fatalf("tests should pass after JSON fallback repair: %v\n%s", err, out)
	}
	st, err := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || !st.JSONFallback {
		t.Fatal("conversation state should remember the JSON fallback so later steps do not retry native tools")
	}
	if strings.Contains(ctl.summary, "sk-scripted") {
		t.Fatalf("summary leaked api key: %q", ctl.summary)
	}
}

func TestDirect_HTTPUnrelatedErrorsDoNotFallback(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"this model does not support tools"}}`},
		{"rate_limit", http.StatusTooManyRequests, `{"error":{"message":"this model does not support tools"}}`},
		{"server", http.StatusInternalServerError, `{"error":{"message":"this model does not support tools"}}`},
		{"context_length", http.StatusBadRequest, `{"error":{"message":"maximum context length exceeded"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work, data := t.TempDir(), t.TempDir()
			var mu sync.Mutex
			n, jsonReqs := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeChatReq(t, r)
				mu.Lock()
				defer mu.Unlock()
				n++
				if len(req.Tools) == 0 {
					jsonReqs++
				}
				writeChatError(w, tc.status, tc.body)
			}))
			t.Cleanup(srv.Close)
			client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
			client.MaxRetries = 1
			cfg := directCfg(t, work, data, "write a file")
			cfg.NativeTools = true
			cfg.SessionID = "sess_direct_unrelated_" + tc.name
			o, err := NewFromGoal(cfg, client, SilentLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(o.Close)
			err = NewDirect(o, nil).Run(context.Background())
			if err == nil {
				t.Fatal("unrelated provider error must fail the run")
			}
			mu.Lock()
			defer mu.Unlock()
			if jsonReqs != 0 {
				t.Fatalf("unrelated error %s triggered a JSON fallback (%d JSON requests)", tc.name, jsonReqs)
			}
			st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if st != nil && (st.Done || st.JSONFallback) {
				t.Fatalf("unrelated error must not mark success or remember a fallback: done=%v fallback=%v", st.Done, st.JSONFallback)
			}
			if _, err := os.Stat(filepath.Join(work, "add.go")); err == nil {
				t.Fatal("no tool should have run after an unrelated HTTP error")
			}
		})
	}
}

func TestDirect_HTTPNativeHistoryRefusesJSONFallback(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(addMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "add.go"), []byte(fixedAdd), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := directCfg(t, work, data, "Follow up after a native tool history.")
	cfg.NativeTools = true
	path := filepath.Join(data, cfg.SessionID+".direct.json")
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: "The user's request for this turn:\nImplement Add"},
	}
	msgs = append(msgs, nativeToolPair("w1", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd}, "wrote add.go")...)
	if err := saveDirectState(path, &directState{Version: 1, Messages: msgs, Step: 2, Done: true, Summary: "Add works"}, cfg.APIKey); err != nil {
		t.Fatal(err)
	}

	var jsonReqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatReq(t, r)
		if len(req.Tools) == 0 {
			jsonReqs++
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "should not have fallen back"}), llm.Usage{TotalTokens: 10})
			return
		}
		writeChatError(w, http.StatusBadRequest, toolsUnsupportedBody)
	}))
	t.Cleanup(srv.Close)
	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	client.MaxRetries = 1
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, nil).Run(context.Background())
	if err == nil {
		t.Fatal("native history plus a tools rejection must fail, not silently fall back")
	}
	if !errors.Is(err, ErrJSONFallbackUnsafe) && !strings.Contains(strings.ToLower(err.Error()), "cannot safely fall back") {
		t.Fatalf("expected a clear unsafe-fallback error, got %v", err)
	}
	if jsonReqs != 0 {
		t.Fatalf("should not rewrite native history into a JSON retry, got %d JSON requests", jsonReqs)
	}
	st, loadErr := loadDirectState(path, cfg.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st != nil && st.Done {
		t.Fatal("unsafe fallback must not mark the transcript done")
	}
	if root := o.tree.GetRoot(); root != nil && root.State == models.TaskStateCompleted {
		t.Fatal("unsafe fallback must not complete the task")
	}
}

func TestDirect_HTTPStreamInterruptDoesNotFallbackOrReplay(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var jsonReqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatReq(t, r)
		if len(req.Tools) == 0 {
			jsonReqs++
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "replayed.txt", "content": "should not exist"}), llm.Usage{TotalTokens: 10})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abrupt end of a successful stream: not a tools-unsupported rejection.
	}))
	t.Cleanup(srv.Close)
	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	client.MaxRetries = 1
	cfg := directCfg(t, work, data, "write replayed.txt")
	cfg.NativeTools = true
	cfg.Stream = true
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, nil).Run(context.Background())
	if _, statErr := os.Stat(filepath.Join(work, "replayed.txt")); statErr == nil {
		t.Fatal("stream interrupt must not replay a later JSON write")
	}
	if jsonReqs != 0 {
		t.Fatalf("stream interrupt triggered a JSON fallback (%d requests)", jsonReqs)
	}
	if err == nil {
		// A cleanly parsed short stream that then finishes as invalid JSON may
		// retry inside the agent, but it must not have downgraded to a write.
		return
	}
}

func TestDirect_HTTPFallbackBudgetAndCancelAreNotSuccess(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatReq(t, r)
			if len(req.Tools) > 0 {
				writeChatError(w, http.StatusBadRequest, toolsUnsupportedBody)
				return
			}
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "finished under budget"}), llm.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100})
		}))
		t.Cleanup(srv.Close)
		client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		client.MaxRetries = 1
		cfg := directCfg(t, work, data, "just finish")
		cfg.NativeTools = true
		cfg.BudgetTokens = 50
		cfg.SessionID = "sess_direct_fallback_budget"
		o, err := NewFromGoal(cfg, client, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		err = NewDirect(o, nil).Run(context.Background())
		if err == nil {
			t.Fatal("token budget after a JSON fallback must stop the run")
		}
		st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if st != nil && st.Done {
			t.Fatal("budget stop must not mark the transcript done")
		}
		if root := o.tree.GetRoot(); root != nil && root.State == models.TaskStateCompleted {
			t.Fatal("budget stop must not complete the task")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatReq(t, r)
			if len(req.Tools) > 0 {
				writeChatError(w, http.StatusBadRequest, toolsUnsupportedBody)
				return
			}
			cancel()
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		client.MaxRetries = 1
		client.HTTPClient = &http.Client{Timeout: 5 * time.Second}
		cfg := directCfg(t, work, data, "just finish")
		cfg.NativeTools = true
		cfg.RequestTimeout = 0
		cfg.SessionID = "sess_direct_fallback_cancel"
		o, err := NewFromGoal(cfg, client, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		err = NewDirect(o, nil).Run(ctx)
		if err == nil {
			t.Fatal("canceled fallback retry must not succeed")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "cancel") {
			t.Fatalf("expected cancel, got %v", err)
		}
		st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if st != nil && st.Done {
			t.Fatal("cancel must not mark the transcript done")
		}
	})
}

// TestDirect_HTTPContextLengthAffirmingToolsDoesNotFallback is the P2/F1
// review fixture: a 400/422 context-length error that also affirms tool
// support must not trigger a JSON retry or persist JSONFallback.
func TestDirect_HTTPContextLengthAffirmingToolsDoesNotFallback(t *testing.T) {
	const body = `{"error":{"message":"Maximum context length exceeded. This model does support tool use.","code":"context_length_exceeded"}}`
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			work, data := t.TempDir(), t.TempDir()
			var mu sync.Mutex
			native, jsonRequests := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeChatReq(t, r)
				mu.Lock()
				defer mu.Unlock()
				if len(req.Tools) > 0 {
					native++
				} else {
					jsonRequests++
				}
				writeChatError(w, status, body)
			}))
			t.Cleanup(srv.Close)
			cfg := directCfg(t, work, data, "Inspect the project.")
			cfg.SessionID = fmt.Sprintf("sess_direct_context_affirm_tools_%d", status)
			cfg.NativeTools = true
			client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
			client.MaxRetries = 1
			o, err := NewFromGoal(cfg, client, SilentLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(o.Close)
			err = NewDirect(o, nil).Run(context.Background())
			if err == nil {
				t.Fatal("context rejection must not complete the task")
			}
			st, loadErr := loadDirectState(filepath.Join(data, cfg.SessionID+".direct.json"), cfg.APIKey)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if native != 1 {
				t.Errorf("HTTP path precondition failed: native requests=%d, want 1", native)
			}
			if jsonRequests != 0 {
				t.Errorf("explicit context error affirming tool support triggered JSON retry (%d)", jsonRequests)
			}
			if st != nil && st.JSONFallback {
				t.Error("unrelated context error persisted a false per-conversation fallback")
			}
			if st != nil && st.Done {
				t.Error("unrelated context error must not mark the transcript done")
			}
		})
	}
}
