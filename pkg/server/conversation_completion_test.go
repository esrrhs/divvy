package server

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// TestConversation_TurnChangesPersistedBeforePhaseDone observes the first
// moment the terminal phase is published, on the serve goroutine, with no
// sleep. The old order published Done before saveTurnChanges/run_finished
// and this hook would fail immediately.
func TestConversation_TurnChangesPersistedBeforePhaseDone(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	type observed struct {
		phase    agent.RunPhase
		finished bool
		added    bool
		changes  []turnChange
		err      error
	}
	saw := make(chan observed, 1)
	t.Cleanup(func() { agent.AfterTerminalPublished = nil })
	agent.AfterTerminalPublished = func(h *agent.RunHandle) {
		out := observed{phase: h.Phase()}
		list, err := listConversations(data)
		if err != nil {
			out.err = err
			saw <- out
			return
		}
		var c *Conversation
		for _, item := range list {
			if containsString(item.SessionIDs, h.SessionID()) || item.CurrentSessionID == h.SessionID() {
				c = item
				break
			}
		}
		if c == nil {
			saw <- out
			return
		}
		changes, cerr := loadTurnChanges(data, c.ID, h.SessionID())
		out.changes = changes
		if cerr != nil && !os.IsNotExist(cerr) {
			out.err = cerr
		}
		for _, ch := range changes {
			if ch.Path == "out.txt" && ch.Status == "added" {
				out.added = true
			}
		}
		for _, m := range c.Messages {
			if m.Kind == kindRunFinished {
				out.finished = true
			}
		}
		saw <- out
	}

	var wrote bool
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			if !wrote {
				wrote = true
				return jsonAct(tools.ToolWriteFile, map[string]any{"path": "out.txt", "content": "hi\n"}), nil
			}
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote out.txt"}), nil
		}}
	})
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "auto",
		"message": "write out file",
	})
	if st != http.StatusCreated && st != http.StatusAccepted {
		t.Fatalf("create status=%d %v", st, resp)
	}
	out := <-saw
	if out.err != nil {
		t.Fatalf("observe: %v", out.err)
	}
	if out.phase != agent.PhaseDone {
		t.Fatalf("hook ran with phase=%s, want done", out.phase)
	}
	if !out.finished || !out.added {
		t.Fatalf("when Done is first published, run_finished and out.txt changes must already be readable: finished=%v added=%v changes=%+v", out.finished, out.added, out.changes)
	}
}
