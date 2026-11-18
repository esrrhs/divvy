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

const (
	contextLengthBody      = `{"error":{"message":"Maximum context length exceeded.","code":"context_length_exceeded"}}`
	contextConstraintTail  = "CONSTRAINT-END-ZZZ-KEEP-ME"
	contextOnceContent     = "once-write-unique"
	contextEarlyBlobMarker = "EARLY-BLOB-UNIQUE-aaaa"
)

func writeNativeCalls(w http.ResponseWriter, calls []llm.ToolCall, usage llm.Usage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"finish_reason": "tool_calls",
			"message":       map[string]any{"role": "assistant", "tool_calls": calls},
		}},
		"usage": usage,
	})
}

func seedContextRetryHistory(native bool) []llm.Message {
	goal := "The user's request for this turn:\nGOAL-KEEP-ME implement Add"
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: goal},
	}
	for i := 0; i < 12; i++ {
		body := ""
		if i == 0 {
			body = contextEarlyBlobMarker + "\n"
		}
		body += strings.Repeat(fmt.Sprintf("blob-%d-xxxx\n", i), 80)
		path := fmt.Sprintf("hist-%d.txt", i)
		if native {
			id := fmt.Sprintf("r-%d", i)
			msgs = append(msgs, nativeToolPair(id, tools.ToolReadFile, map[string]any{"path": path}, body)...)
		} else {
			payload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": tools.ToolReadFile, "args": map[string]any{"path": path}})
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: string(payload)},
				llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("Tool %s result:\n%s", tools.ToolReadFile, body)},
			)
		}
	}
	if native {
		msgs = append(msgs, nativeToolPair("w-once", tools.ToolWriteFile, map[string]any{"path": "once.txt", "content": contextOnceContent}, "wrote once.txt")...)
		msgs = append(msgs, nativeToolPair("ask-1", "ask", map[string]any{"question": "any extra constraint?"}, "User answer:\nKeep the end of this answer visible: "+contextConstraintTail)...)
	} else {
		wPayload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": tools.ToolWriteFile, "args": map[string]any{"path": "once.txt", "content": contextOnceContent}})
		aPayload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": "ask", "args": map[string]any{"question": "any extra constraint?"}})
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: string(wPayload)},
			llm.Message{Role: llm.RoleUser, Content: "Tool write_file result:\nwrote once.txt"},
			llm.Message{Role: llm.RoleAssistant, Content: string(aPayload)},
			llm.Message{Role: llm.RoleUser, Content: "User answer:\nKeep the end of this answer visible: " + contextConstraintTail},
		)
	}
	return msgs
}

func assertNoFakeTruncation(t *testing.T, req llm.Request) {
	t.Helper()
	joined := joinReq(req)
	if strings.Contains(joined, contextEarlyBlobMarker) && !strings.Contains(joined, "blob-0-xxxx") {
		t.Error("early tool result was silently truncated and presented as the original contents")
	}
	if orphans := orphanToolResults(req.Messages); len(orphans) > 0 {
		t.Errorf("shrunk request orphaned tool results: %s", strings.Join(orphans, ","))
	}
}

func assertCorePreserved(t *testing.T, req llm.Request) {
	t.Helper()
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem || !strings.Contains(req.Messages[0].Content, directMarker) {
		t.Errorf("request lost the system prompt")
	}
	joined := joinReq(req)
	if !strings.Contains(joined, "GOAL-KEEP-ME") {
		t.Error("request lost the original goal")
	}
	if !strings.Contains(joined, contextConstraintTail) {
		t.Error("request dropped the trailing constraint of the latest human answer")
	}
}

func scriptContextRepair(t *testing.T, native bool, req llm.Request, step *int, usage llm.Usage, w http.ResponseWriter) {
	t.Helper()
	joined := joinReq(req)
	*step++
	switch *step {
	case 1:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("w-mod", tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod}), usage)
		}
	case 2:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("w-add", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd}), usage)
		}
	case 3:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("w-test", tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest}), usage)
		}
	case 4:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("t-fail", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), usage)
		}
	case 5:
		if !strings.Contains(joined, "FAIL") {
			t.Error("repair step did not see the failing test output")
		}
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("w-fix", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd}), usage)
		}
	case 6:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("t-pass", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), usage)
		}
	default:
		if native {
			writeNativeCalls(w, []llm.ToolCall{nativeTC("f-done", tools.ToolFinish, map[string]any{"summary": "Add now adds; go test passed after the first failure."})}, usage)
		} else {
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "Add now adds; go test passed after the first failure."}), usage)
		}
	}
}

func TestDirect_HTTPContextLengthShrinksAndRepairs(t *testing.T) {
	for _, native := range []bool{true, false} {
		name := "json"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			work, data := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(work, "once.txt"), []byte(contextOnceContent), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := directCfg(t, work, data, "Continue the Add implementation.")
			cfg.SessionID = fmt.Sprintf("sess_direct_ctx_repair_%s", name)
			cfg.NativeTools = native
			cfg.DirectContextChars = 200000
			cfg.Stream = false
			msgs := seedContextRetryHistory(native)
			path := filepath.Join(data, cfg.SessionID+".direct.json")
			st := &directState{Version: 1, Messages: msgs, Step: 0, JSONFallback: !native}
			if err := saveDirectState(path, st, cfg.APIKey); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			var sizes []int
			var contextRejects, accepted int
			var firstAccepted, firstRejected int
			var laterRejected bool
			step := 0
			usage := llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}
			var sawConstraintInRetry bool

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeChatReq(t, r)
				n := directWindowChars(req.Messages)
				mu.Lock()
				defer mu.Unlock()
				sizes = append(sizes, n)
				if len(sizes) == 1 || (firstAccepted == 0 && n >= sizes[0]) {
					contextRejects++
					firstRejected = n
					writeChatError(w, http.StatusBadRequest, contextLengthBody)
					return
				}
				if firstAccepted == 0 {
					firstAccepted = n
					if n >= firstRejected {
						t.Errorf("retry was not smaller than the rejected request (%d >= %d)", n, firstRejected)
					}
					assertCorePreserved(t, req)
					assertNoFakeTruncation(t, req)
					if strings.Contains(joinReq(req), contextConstraintTail) {
						sawConstraintInRetry = true
					}
					if native && len(req.Tools) == 0 {
						t.Error("native shrink retry dropped tool definitions")
					}
					if !native && len(req.Tools) > 0 {
						t.Error("JSON shrink retry unexpectedly sent native tools")
					}
				} else if n >= firstRejected {
					laterRejected = true
					contextRejects++
					writeChatError(w, http.StatusBadRequest, contextLengthBody)
					return
				}
				accepted++
				scriptContextRepair(t, native, req, &step, usage, w)
			}))
			t.Cleanup(srv.Close)

			client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
			client.MaxRetries = 1
			o, err := NewFromGoal(cfg, client, SilentLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(o.Close)
			if err := NewDirect(o, nil).Run(context.Background()); err != nil {
				t.Fatalf("context shrink repair run: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if contextRejects == 0 {
				t.Fatal("provider never rejected a context-length request")
			}
			if firstAccepted == 0 || accepted == 0 {
				t.Fatalf("no shrunk request was accepted (rejects=%d accepted=%d sizes=%v)", contextRejects, accepted, sizes)
			}
			if !sawConstraintInRetry {
				t.Fatal("shrunk retry lost the latest answer's trailing constraint")
			}
			if laterRejected {
				t.Fatal("later steps in the same run grew back over the rejected size")
			}
			got, err := os.ReadFile(filepath.Join(work, "once.txt"))
			if err != nil || string(got) != contextOnceContent {
				t.Fatalf("already-executed write was replayed or lost: %q %v", got, err)
			}
			if out, err := runGoTest(t, work); err != nil {
				t.Fatalf("tests should pass after shrink+repair: %v\n%s", err, out)
			}
			saved, err := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
			if err != nil || saved == nil {
				t.Fatalf("load transcript: %v", err)
			}
			full := ""
			for _, m := range saved.Messages {
				full += m.Content
			}
			if !strings.Contains(full, contextEarlyBlobMarker) || !strings.Contains(full, "blob-0-xxxx") {
				t.Fatal("on-disk transcript lost the full early tool result")
			}
			if !saved.Done {
				t.Fatal("successful repair must mark the transcript done")
			}
			if saved.JSONFallback != !native {
				t.Fatalf("context shrink must not flip JSON fallback: got %v", saved.JSONFallback)
			}
		})
	}
}

func TestDirect_HTTPContextLengthUnshrinkableAndBounded(t *testing.T) {
	t.Run("unshrinkable_core", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		huge := "GOAL-KEEP-ME " + strings.Repeat("CORE-CONSTRAINT-xxxxx\n", 400) + contextConstraintTail
		cfg := directCfg(t, work, data, huge)
		cfg.SessionID = "sess_direct_ctx_unshrinkable"
		cfg.NativeTools = true
		cfg.DirectContextChars = 200000
		var mu sync.Mutex
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n++
			mu.Unlock()
			writeChatError(w, http.StatusUnprocessableEntity, contextLengthBody)
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
			t.Fatal("incompressible core must fail clearly")
		}
		if !errors.Is(err, ErrContextUnshrinkable) && !strings.Contains(strings.ToLower(err.Error()), "cannot be shrunk") && !strings.Contains(strings.ToLower(err.Error()), "cannot shrink") {
			t.Fatalf("expected unshrinkable context error, got %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if n < 1 {
			t.Fatal("expected at least the rejected request")
		}
		if n > 5 {
			t.Fatalf("unshrinkable retry was not bounded: %d requests", n)
		}
		st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if st != nil && (st.Done || st.JSONFallback) {
			t.Fatalf("unshrinkable failure must not succeed or flip protocol: done=%v fallback=%v", st.Done, st.JSONFallback)
		}
	})

	t.Run("bounded_repeated_reject", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "once.txt"), []byte(contextOnceContent), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := directCfg(t, work, data, "Continue the Add implementation.")
		cfg.SessionID = "sess_direct_ctx_bounded"
		cfg.NativeTools = true
		cfg.DirectContextChars = 200000
		if err := saveDirectState(filepath.Join(data, cfg.SessionID+".direct.json"), &directState{Version: 1, Messages: seedContextRetryHistory(true)}, cfg.APIKey); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n++
			mu.Unlock()
			writeChatError(w, http.StatusBadRequest, contextLengthBody)
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
			t.Fatal("repeated context rejections must fail")
		}
		mu.Lock()
		defer mu.Unlock()
		if n < 2 {
			t.Fatalf("expected shrink retries, got %d requests", n)
		}
		if n > 5 {
			t.Fatalf("repeated context rejection was not bounded: %d requests", n)
		}
		st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if st != nil && st.Done {
			t.Fatal("bounded context failure must not mark the transcript done")
		}
		got, _ := os.ReadFile(filepath.Join(work, "once.txt"))
		if string(got) != contextOnceContent {
			t.Fatalf("repeated rejects replayed the earlier write: %q", got)
		}
	})
}

func TestDirect_HTTPContextLengthCancelBudgetAndUnrelated(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		cfg := directCfg(t, work, data, "just finish")
		cfg.SessionID = "sess_direct_ctx_budget"
		cfg.NativeTools = true
		cfg.BudgetTokens = 50
		cfg.DirectContextChars = 200000
		if err := saveDirectState(filepath.Join(data, cfg.SessionID+".direct.json"), &directState{Version: 1, Messages: seedContextRetryHistory(true)}, cfg.APIKey); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var first int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatReq(t, r)
			n := directWindowChars(req.Messages)
			mu.Lock()
			defer mu.Unlock()
			if first == 0 {
				first = n
				writeChatError(w, http.StatusBadRequest, contextLengthBody)
				return
			}
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "finished under budget"}), llm.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100})
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
			t.Fatal("token budget must not report a successful finish")
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
		cfg := directCfg(t, work, data, "just finish")
		cfg.SessionID = "sess_direct_ctx_cancel"
		cfg.NativeTools = true
		cfg.RequestTimeout = 0
		cfg.DirectContextChars = 200000
		if err := saveDirectState(filepath.Join(data, cfg.SessionID+".direct.json"), &directState{Version: 1, Messages: seedContextRetryHistory(true)}, cfg.APIKey); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		var mu sync.Mutex
		var first int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatReq(t, r)
			n := directWindowChars(req.Messages)
			mu.Lock()
			if first == 0 {
				first = n
				mu.Unlock()
				writeChatError(w, http.StatusBadRequest, contextLengthBody)
				return
			}
			mu.Unlock()
			cancel()
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		client.MaxRetries = 1
		client.HTTPClient = &http.Client{Timeout: 5 * time.Second}
		o, err := NewFromGoal(cfg, client, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		err = NewDirect(o, nil).Run(ctx)
		if err == nil {
			t.Fatal("canceled context shrink retry must not succeed")
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

	t.Run("unrelated_errors", func(t *testing.T) {
		cases := []struct {
			name   string
			status int
			body   string
		}{
			{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`},
			{"forbidden", http.StatusForbidden, `{"error":{"message":"maximum context length exceeded"}}`},
			{"rate_limit", http.StatusTooManyRequests, `{"error":{"message":"maximum context length exceeded"}}`},
			{"server", http.StatusInternalServerError, `{"error":{"message":"maximum context length exceeded"}}`},
			{"generic400", http.StatusBadRequest, `{"error":{"message":"invalid request"}}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				work, data := t.TempDir(), t.TempDir()
				cfg := directCfg(t, work, data, "Inspect the project.")
				cfg.SessionID = "sess_direct_ctx_unrelated_" + tc.name
				cfg.NativeTools = true
				var mu sync.Mutex
				n, smaller := 0, 0
				var first int
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					req := decodeChatReq(t, r)
					size := directWindowChars(req.Messages)
					mu.Lock()
					defer mu.Unlock()
					n++
					if first == 0 {
						first = size
					} else if size < first {
						smaller++
					}
					writeChatError(w, tc.status, tc.body)
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
					t.Fatal("unrelated error must fail the run")
				}
				mu.Lock()
				defer mu.Unlock()
				if smaller != 0 {
					t.Fatalf("unrelated error %s triggered a smaller-window retry (%d)", tc.name, smaller)
				}
				st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if st != nil && st.Done {
					t.Fatal("unrelated error must not mark the transcript done")
				}
			})
		}
	})

	t.Run("stream_interrupt", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		cfg := directCfg(t, work, data, "write replayed.txt")
		cfg.SessionID = "sess_direct_ctx_stream"
		cfg.NativeTools = true
		cfg.Stream = true
		var mu sync.Mutex
		n, smaller := 0, 0
		var first int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatReq(t, r)
			size := directWindowChars(req.Messages)
			mu.Lock()
			n++
			if first == 0 {
				first = size
			} else if size < first {
				smaller++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))
		t.Cleanup(srv.Close)
		client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		client.MaxRetries = 1
		client.HTTPClient = &http.Client{Timeout: 5 * time.Second}
		o, err := NewFromGoal(cfg, client, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		_ = NewDirect(o, nil).Run(context.Background())
		mu.Lock()
		defer mu.Unlock()
		if smaller != 0 {
			t.Fatalf("stream interrupt triggered a smaller-window retry (%d of %d requests)", smaller, n)
		}
		if _, statErr := os.Stat(filepath.Join(work, "replayed.txt")); statErr == nil {
			t.Fatal("stream interrupt must not apply a later write")
		}
	})
}

// TestDirect_HTTPContextShrinkPartialStreamMustNotReplay is the independent
// P2/F1 fixture: after a context-limit shrink, a streamed request that has
// already delivered text and then hits a truncated body must not be replayed
// by the default OpenAI client (MaxRetries=0).
func TestDirect_HTTPContextShrinkPartialStreamMustNotReplay(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "continue task")
	cfg.SessionID = "sess_direct_ctx_partial_stream"
	cfg.NativeTools = true
	cfg.Stream = true
	cfg.DirectContextChars = 200000
	path := filepath.Join(data, cfg.SessionID+".direct.json")
	if err := saveDirectState(path, &directState{Version: 1, Messages: seedContextRetryHistory(true)}, cfg.APIKey); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	var sizes []int
	var streams []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatReq(t, r)
		mu.Lock()
		defer mu.Unlock()
		n++
		sizes = append(sizes, directWindowChars(req.Messages))
		streams = append(streams, req.Stream)
		switch n {
		case 1, 2:
			writeChatError(w, http.StatusBadRequest, contextLengthBody)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial accepted output\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		default:
			writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "must not be accepted after partial stream failure"}), llm.Usage{})
		}
	}))
	t.Cleanup(srv.Close)
	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	client.MaxBackoff = time.Millisecond
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, nil).Run(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) < 3 || sizes[2] >= sizes[0] || !streams[2] {
		t.Fatalf("did not reach the intended smaller streamed recovery request: n=%d sizes=%v streams=%v", n, sizes, streams)
	}
	if n != 3 {
		t.Errorf("accepted partial output / truncated body replayed request: got %d HTTP calls, want exactly 3 (two initial protocol probes + one shrunk request)", n)
	}
	if err == nil {
		t.Fatal("network failure after partial output must stop this run, not accept replayed finish")
	}
	if !errors.Is(err, llm.ErrStreamInterrupted) && !strings.Contains(strings.ToLower(err.Error()), "stream interrupted") {
		t.Fatalf("expected a stream-interrupt error, got %v", err)
	}
	saved, loadErr := loadDirectState(path, cfg.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if saved != nil && saved.Done {
		t.Error("partial-output failure must not be persisted as successful completion")
	}
}

// TestDirect_HTTPJSONFallbackPartialStreamMustNotReplay covers the same
// transport boundary after a tools-unsupported JSON fallback.
func TestDirect_HTTPJSONFallbackPartialStreamMustNotReplay(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "just finish")
	cfg.SessionID = "sess_direct_fallback_partial_stream"
	cfg.NativeTools = true
	cfg.Stream = true
	var mu sync.Mutex
	n := 0
	var streams []bool
	var jsonAfterOutput int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatReq(t, r)
		mu.Lock()
		defer mu.Unlock()
		n++
		streams = append(streams, req.Stream)
		if len(req.Tools) > 0 {
			writeChatError(w, http.StatusBadRequest, toolsUnsupportedBody)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial accepted output\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		jsonAfterOutput++
		writeChatJSON(w, jsonActContent(tools.ToolFinish, map[string]any{"summary": "must not be accepted after partial stream failure"}), llm.Usage{TotalTokens: 10})
	}))
	t.Cleanup(srv.Close)
	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	client.MaxBackoff = time.Millisecond
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, nil).Run(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if jsonAfterOutput != 0 {
		t.Errorf("JSON fallback stream replayed after partial output (%d non-stream JSON requests); calls=%d streams=%v", jsonAfterOutput, n, streams)
	}
	if err == nil {
		t.Fatal("partial stream after JSON fallback must not finish successfully")
	}
	st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st != nil && st.Done {
		t.Error("partial-output failure must not be persisted as successful completion")
	}
}
