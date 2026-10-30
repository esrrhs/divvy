package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatStreamCapturesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		if !strings.Contains(string(body), `"stream_options":{"include_usage":true}`) {
			t.Errorf("request missing stream_options: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewOpenAIClient("key", srv.URL, 5*time.Second)
	resp, err := client.Chat(context.Background(), Request{
		Model:    "test",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" {
		t.Fatalf("content %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 18 || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("usage not captured from final chunk: %+v", resp.Usage)
	}
}

func TestChatSetsClientIdentificationHeaders(t *testing.T) {
	var gotUA, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	client := NewOpenAIClient("key", srv.URL, 5*time.Second)
	if client.UserAgent != "divvy" {
		t.Fatalf("default UserAgent = %q, want divvy", client.UserAgent)
	}
	_, err := client.Chat(context.Background(), Request{
		Model:    "test",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "divvy" {
		t.Fatalf("User-Agent = %q, want divvy", gotUA)
	}
	if gotTitle != "divvy" {
		t.Fatalf("X-Title = %q, want divvy", gotTitle)
	}

	// Empty UserAgent still falls back to divvy on the wire.
	gotUA, gotTitle = "", ""
	client.UserAgent = ""
	_, err = client.Chat(context.Background(), Request{
		Model:    "test",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "divvy" || gotTitle != "divvy" {
		t.Fatalf("empty UserAgent fallback: UA=%q X-Title=%q", gotUA, gotTitle)
	}

	// Custom override applies to both headers.
	gotUA, gotTitle = "", ""
	client.UserAgent = "divvy-custom"
	_, err = client.Chat(context.Background(), Request{
		Model:    "test",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "divvy-custom" || gotTitle != "divvy-custom" {
		t.Fatalf("custom UserAgent: UA=%q X-Title=%q", gotUA, gotTitle)
	}
}
