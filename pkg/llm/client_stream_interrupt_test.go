package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeShortSSE(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Content-Length", "10000")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func decodeChatBody(t *testing.T, r *http.Request) Request {
	t.Helper()
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return req
}

func TestChatDoesNotReplayAfterStreamedText(t *testing.T) {
	var mu sync.Mutex
	n := 0
	var streams []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatBody(t, r)
		mu.Lock()
		n++
		streams = append(streams, req.Stream)
		mu.Unlock()
		if req.Stream {
			writeShortSSE(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial accepted output\"}}]}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"replayed finish"}}]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	c.MaxBackoff = time.Millisecond
	resp, err := c.Chat(context.Background(), Request{
		Model:    "scripted",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	})
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Errorf("replayed after streamed text: HTTP calls=%d streams=%v, want 1", n, streams)
	}
	if err == nil {
		t.Fatalf("partial stream must be an error, got resp=%+v", resp)
	}
	if !errors.Is(err, ErrStreamInterrupted) {
		t.Errorf("error should be ErrStreamInterrupted, got %v", err)
	}
	if resp != nil {
		t.Errorf("must not return a successful response after a truncated stream: %+v", resp)
	}
	if strings.Contains(err.Error(), "sk-scripted") {
		t.Errorf("error leaked the API key: %v", err)
	}
}

func TestChatDoesNotReplayAfterToolCallFragment(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatBody(t, r)
		n++
		if req.Stream {
			writeShortSSE(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"p\"}}]}}]}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	c.MaxBackoff = time.Millisecond
	_, err := c.Chat(context.Background(), Request{
		Model:    "scripted",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	})
	if n != 1 {
		t.Errorf("replayed after tool-call fragment: HTTP calls=%d, want 1", n)
	}
	if !errors.Is(err, ErrStreamInterrupted) {
		t.Errorf("error should be ErrStreamInterrupted, got %v", err)
	}
}

func TestChatUsageOrEmptyDeltaIsNotMeaningfulOutput(t *testing.T) {
	t.Run("usage_only", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatBody(t, r)
			n++
			if req.Stream {
				writeShortSSE(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":0,\"total_tokens\":3}}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"recovered without prior output"}}]}`)
		}))
		t.Cleanup(srv.Close)
		c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		c.MaxBackoff = time.Millisecond
		resp, err := c.Chat(context.Background(), Request{
			Model:    "scripted",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
			Stream:   true,
		})
		if err != nil {
			t.Fatalf("usage-only stream failure should keep the no-output fallback: %v", err)
		}
		if resp == nil || resp.Content != "recovered without prior output" {
			t.Fatalf("content = %+v", resp)
		}
		if n != 2 {
			t.Errorf("usage-only should allow one non-stream probe, got %d calls", n)
		}
	})

	t.Run("empty_delta", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeChatBody(t, r)
			n++
			if req.Stream {
				writeShortSSE(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"recovered without prior output"}}]}`)
		}))
		t.Cleanup(srv.Close)
		c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
		c.MaxBackoff = time.Millisecond
		resp, err := c.Chat(context.Background(), Request{
			Model:    "scripted",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
			Stream:   true,
		})
		if err != nil {
			t.Fatalf("empty delta should not count as delivered output: %v", err)
		}
		if resp == nil || resp.Content != "recovered without prior output" {
			t.Fatalf("content = %+v", resp)
		}
		if n != 2 {
			t.Errorf("empty delta should allow one non-stream probe, got %d calls", n)
		}
	})
}

func TestChatCompleteStreamStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	resp, err := c.Chat(context.Background(), Request{
		Model:    "scripted",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestChatCancelDuringStreamIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan struct{})
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		select {
		case <-started:
		default:
			close(started)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 5*time.Second)
	c.MaxBackoff = time.Millisecond
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, Request{
			Model:    "scripted",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
			Stream:   true,
		})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not see the stream request")
	}
	cancel()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancel")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		t.Fatalf("expected cancel, got %v", err)
	}
	if n != 1 {
		t.Errorf("canceled stream was retried: %d calls", n)
	}
}
