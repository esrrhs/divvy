package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

type auditBlockBody struct {
	once             sync.Once
	entered, release chan struct{}
	*strings.Reader
}

func (b *auditBlockBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.Reader.Read(p)
}

func (b *auditBlockBody) Close() error { return nil }

var _ io.ReadCloser = (*auditBlockBody)(nil)

func TestAuditStaleInjectAckClearsNewInstruction(t *testing.T) {
	data := t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(time.Second)
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConversation(t.TempDir(), ConversationSettings{AgentMode: agent.AgentLoopDirect}, "audit")
	if err != nil {
		t.Fatal(err)
	}
	c.Steer = &PendingSteer{Text: "newer-important-instruction", InjectID: "inj_new"}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	ch := make(chan map[string]any, 1)
	ch <- map[string]any{"kind": "user_inject", "instruction": "older-instruction", "inject_id": "inj_old"}
	close(ch)
	s.forwardDirectEvents(c.ID, ch)
	c, err = loadConversation(data, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Steer == nil || c.Steer.Text != "newer-important-instruction" {
		t.Fatalf("stale inject ack cleared a newer pending steer: %+v", c.Steer)
	}
}

func TestAuditHunksRaceNewRun(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(time.Second)
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConversation(work, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "audit")
	if err != nil {
		t.Fatal(err)
	}
	sid := "sess_audit_old"
	c.CurrentSessionID = sid
	c.SessionIDs = []string{sid}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "x"), []byte("new\n"), 0644)
	if err := saveTurnChanges(data, c.ID, sid, []turnChange{{Path: "x", Status: "modified", Old: "old\n", New: "new\n"}}, nil); err != nil {
		t.Fatal(err)
	}
	b := &auditBlockBody{entered: make(chan struct{}), release: make(chan struct{}), Reader: strings.NewReader(`{"path":"x","all":"reject"}`)}
	req := httptest.NewRequest("POST", "http://localhost/", nil)
	req.Body = b
	req.SetPathValue("id", c.ID)
	req.SetPathValue("sessionId", sid)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.handleApplyTurnHunks(rec, req) }()
	<-b.entered
	cfg := agent.DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.Goal = "new run"
	cfg.SessionID = "sess_audit_new"
	cfg.AgentLoop = agent.AgentLoopDirect
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	h, err := m.Start(cfg, client, agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.CurrentSessionID = h.SessionID()
	c.SessionIDs = append(c.SessionIDs, h.SessionID())
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	<-done
	v, err := os.ReadFile(filepath.Join(work, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("stale hunk POST succeeded after a new run started; content=%q", v)
	}
	if string(v) != "new\n" {
		t.Fatalf("workdir was overwritten by the stale hunk POST: %q", v)
	}
	if h.Phase().Terminal() {
		t.Fatal("new run is not active")
	}
}

func TestAuditEmptyFileHunkDeletesFile(t *testing.T) {
	work := t.TempDir()
	p := filepath.Join(work, "x")
	if err := os.WriteFile(p, []byte("nonempty"), 0644); err != nil {
		t.Fatal(err)
	}
	ch := turnChange{Path: "x", Status: "modified", Old: "nonempty", New: ""}
	if err := applyTurnResult(work, ch, "", "accepted"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("empty-file hunk deleted the file: %v", err)
	}
	if string(got) != "" {
		t.Fatalf("empty file content=%q", got)
	}
}
