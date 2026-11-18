package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

func postSettings(t *testing.T, s *Server, convID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost/", strings.NewReader(body))
	req.SetPathValue("id", convID)
	rec := httptest.NewRecorder()
	s.handleUpdateConversationSettings(rec, req)
	return rec
}

// TestConversationSettingsUpdate_SwitchModel covers the mid-conversation
// model switch: values persist, the transcript records the change, and a
// no-op does not spam a notice.
func TestConversationSettingsUpdate_SwitchModel(t *testing.T) {
	data := t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(0)
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConversation(t.TempDir(), ConversationSettings{AgentMode: agent.AgentLoopDirect, Model: "phi4-mini:latest"}, "switch")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}

	rec := postSettings(t, s, c.ID, `{"model":"qwen3.8:27b","base_url":"http://127.0.0.1:11434/v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch failed: %d %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Settings ConversationSettings `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Settings.Model != "qwen3.8:27b" || view.Settings.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("settings not persisted: %+v", view.Settings)
	}

	latest, err := loadConversation(data, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := latest.Messages[len(latest.Messages)-1]
	if last.Outcome != "model_switched" || !strings.Contains(last.Text, "qwen3.8:27b") {
		t.Fatalf("switch notice missing: %+v", last)
	}

	// A no-op switch (same values) must not append another notice.
	noticesBefore := 0
	for _, msg := range latest.Messages {
		if msg.Outcome == "model_switched" {
			noticesBefore++
		}
	}
	if rec := postSettings(t, s, c.ID, `{"model":"qwen3.8:27b"}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op switch failed: %d %s", rec.Code, rec.Body.String())
	}
	latest, err = loadConversation(data, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	noticesAfter := 0
	for _, msg := range latest.Messages {
		if msg.Outcome == "model_switched" {
			noticesAfter++
		}
	}
	if noticesAfter != noticesBefore {
		t.Fatalf("no-op switch appended a notice: %d -> %d", noticesBefore, noticesAfter)
	}
}

// TestConversationSettingsUpdate_Guards covers validation and the live-run
// refusal: a paused conversation may switch (the resume picks it up), but an
// executing one may not.
func TestConversationSettingsUpdate_Guards(t *testing.T) {
	data := t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(0)
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	c, err := newConversation(work, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "guards")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}

	// Invalid base URL.
	if rec := postSettings(t, s, c.ID, `{"base_url":"ftp://x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad base_url must 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// Both empty.
	if rec := postSettings(t, s, c.ID, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty update must 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// A live (blocked) run must lock the settings.
	cfg := agent.DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.Goal = "blocked run"
	cfg.AgentLoop = agent.AgentLoopDirect
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	h, err := m.Start(cfg, client, agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Abort() }()
	c.CurrentSessionID = h.SessionID()
	c.SessionIDs = append(c.SessionIDs, h.SessionID())
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}

	rec := postSettings(t, s, c.ID, `{"model":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("live run must lock settings, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "还在执行") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
}

// TestConversationRename covers the sidebar rename: values persist (redacted
// on save like every title), empty/overlong titles are refused, and the view
// comes back with the new name.
func TestConversationRename(t *testing.T) {
	data := t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(0)
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConversation(t.TempDir(), ConversationSettings{AgentMode: agent.AgentLoopDirect}, "自动标题")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}

	rename := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "http://localhost/", strings.NewReader(body))
		req.SetPathValue("id", c.ID)
		rec := httptest.NewRecorder()
		s.handleRenameConversation(rec, req)
		return rec
	}

	if rec := rename(`{"title":"登录修复：超时分支"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename failed: %d %s", rec.Code, rec.Body.String())
	}
	latest, err := loadConversation(data, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Title != "登录修复：超时分支" {
		t.Fatalf("title not persisted: %q", latest.Title)
	}

	if rec := rename(`{"title":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty title must 400, got %d: %s", rec.Code, rec.Body.String())
	}
	long := strings.Repeat("长", 201)
	if rec := rename(`{"title":"` + long + `"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong title must 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
