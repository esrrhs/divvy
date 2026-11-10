package server

// Regression copy of the independent retest (forward safety assertions).
// Source: /workspace/divvy-audit/retest/server_retest.go — that tree is not modified.
import (
	"context"
	"errors"
	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegressionRetestInjectAckIdentityPositive(t *testing.T) {
	data := t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(time.Second)
	s, e := New(m, data)
	if e != nil {
		t.Fatal(e)
	}
	c, e := newConversation(t.TempDir(), ConversationSettings{AgentMode: agent.AgentLoopDirect}, "retest")
	if e != nil {
		t.Fatal(e)
	}
	c.Steer = &PendingSteer{Text: "new-instruction", InjectID: "new-id"}
	saveConversation(data, c, nil)
	run := func(id string) {
		ch := make(chan map[string]any, 1)
		ch <- map[string]any{"kind": "user_inject", "inject_id": id, "instruction": "arbitrary"}
		close(ch)
		s.forwardDirectEvents(c.ID, ch)
	}
	run("old-id")
	c, e = loadConversation(data, c.ID)
	if e != nil || c.Steer == nil || c.Steer.InjectID != "new-id" {
		t.Fatal("stale ACK cleared new steer")
	}
	run("new-id")
	c, e = loadConversation(data, c.ID)
	if e != nil || c.Steer != nil {
		t.Fatal("matching ACK did not clear steer")
	}
	t.Log("old ACK preserved new steer; matching ID consumed it")
}
func TestRegressionRetestWriteLeaseBothDirectionsPositive(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	m := agent.NewRunManager()
	defer m.Shutdown(time.Second)
	release, e := m.BeginWorkdirWrite(work, "retest")
	if e != nil {
		t.Fatal(e)
	}
	cfg := agent.DefaultConfig()
	cfg.AgentLoop = agent.AgentLoopDirect
	cfg.Goal = "lease"
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "sess_retest_lease"
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, r llm.Request) (*llm.Response, error) { <-ctx.Done(); return nil, ctx.Err() }}
	if _, e = m.Start(cfg, client, agent.SilentLogger()); !errors.Is(e, agent.ErrWorkdirBusy) {
		t.Fatalf("Start ignored write lease: %v", e)
	}
	release()
	h, e := m.Start(cfg, client, agent.SilentLogger())
	if e != nil {
		t.Fatal(e)
	}
	if unlock, e := m.BeginWorkdirWrite(work, "retest"); e == nil {
		unlock()
		t.Fatal("write ignored active run")
	}
	if h.Phase().Terminal() {
		t.Fatal("run not active")
	}
	t.Log("Start blocked by write lease; write blocked by active run")
}
func TestRegressionRetestEmptyExistenceMatrixPositive(t *testing.T) {
	work := t.TempDir()
	cases := []struct {
		status, decision string
		exists           bool
	}{{"modified", "accepted", true}, {"added", "accepted", true}, {"added", "rejected", false}, {"deleted", "accepted", false}, {"deleted", "rejected", true}}
	for _, c := range cases {
		p := filepath.Join(work, "x")
		os.WriteFile(p, []byte(""), 0644)
		if e := applyTurnResult(work, turnChange{Path: "x", Status: c.status}, "", c.decision); e != nil {
			t.Fatal(e)
		}
		i, e := os.Stat(p)
		if c.exists {
			if e != nil || i.Size() != 0 {
				t.Fatalf("%s %s should preserve empty file", c.status, c.decision)
			}
		} else if !os.IsNotExist(e) {
			t.Fatalf("%s %s should delete", c.status, c.decision)
		}
	}
}
