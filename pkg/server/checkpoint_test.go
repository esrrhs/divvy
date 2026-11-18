package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
)

// checkpointTurnFixture builds a three-turn conversation over one file with
// the workdir at turn 3's end state: A →(t1)→ B →(t2)→ C →(t3)→ D.
func checkpointTurnFixture(t *testing.T) (*Server, *Conversation, string, string) {
	t.Helper()
	work, data := t.TempDir(), t.TempDir()
	m := agent.NewRunManager()
	t.Cleanup(func() { m.Shutdown(0) })
	s, err := New(m, data)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConversation(work, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	c.SessionIDs = []string{"s1", "s2", "s3"}
	c.CurrentSessionID = "s3"
	stages := [][3]string{{"s1", "A\n", "B\n"}, {"s2", "B\n", "C\n"}, {"s3", "C\n", "D\n"}}
	for _, st := range stages {
		if err := saveTurnChanges(data, c.ID, st[0], []turnChange{{Path: "f.txt", Status: "modified", Old: st[1], New: st[2]}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("D\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	return s, c, work, data
}

func postCheckpoint(t *testing.T, s *Server, convID, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost/", strings.NewReader(`{"confirm":true}`))
	req.SetPathValue("id", convID)
	req.SetPathValue("sessionId", sid)
	rec := httptest.NewRecorder()
	s.handleCheckpointTurn(rec, req)
	return rec
}

func TestCheckpointTurn_RestoresWorkdirToTurnEnd(t *testing.T) {
	s, c, work, _ := checkpointTurnFixture(t)

	rec := postCheckpoint(t, s, c.ID, "s1")
	if rec.Code != http.StatusOK {
		t.Fatalf("checkpoint failed: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Ok       bool     `json:"ok"`
		Restored []string `json:"restored"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Ok {
		t.Fatalf("ok=false: %s", rec.Body.String())
	}
	// Reverse order: the last turn is undone first.
	if strings.Join(got.Restored, ",") != "s3,s2" {
		t.Fatalf("restored order wrong: %v", got.Restored)
	}

	// The workspace is at turn 1's end state.
	b, err := os.ReadFile(filepath.Join(work, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "B\n" {
		t.Fatalf("workdir not at turn-1 end state: %q", b)
	}

	latest, err := loadConversation(s.dataDir, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"s2", "s3"} {
		if !containsString(latest.RolledBackTurns, sid) {
			t.Fatalf("turn %s not marked rolled back: %v", sid, latest.RolledBackTurns)
		}
	}
	if containsString(latest.RolledBackTurns, "s1") {
		t.Fatal("target turn itself must stay intact")
	}
	last := latest.Messages[len(latest.Messages)-1]
	if last.Outcome != "checkpoint_restored" || last.SessionID != "s1" {
		t.Fatalf("checkpoint marker missing: %+v", last)
	}
}

func TestCheckpointTurn_TargetIsLatestIsANoOp(t *testing.T) {
	s, c, _, _ := checkpointTurnFixture(t)

	rec := postCheckpoint(t, s, c.ID, "s3")
	if rec.Code != http.StatusConflict {
		t.Fatalf("restore to latest turn must conflict, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "没有需要撤销") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
}

func TestCheckpointTurn_RepeatedRestoreIsRefused(t *testing.T) {
	s, c, _, _ := checkpointTurnFixture(t)

	if rec := postCheckpoint(t, s, c.ID, "s1"); rec.Code != http.StatusOK {
		t.Fatalf("first restore failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := postCheckpoint(t, s, c.ID, "s1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second restore must conflict, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCheckpointTurn_StopsOnExternallyEditedFile(t *testing.T) {
	s, c, work, _ := checkpointTurnFixture(t)

	// An external edit after turn 3: turn 3's recorded post no longer
	// matches, so the sequence must refuse without touching anything.
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("external\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rec := postCheckpoint(t, s, c.ID, "s1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("externally edited workdir must stop the restore, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "不一致") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(work, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "external\n" {
		t.Fatalf("external edit was overwritten: %q", b)
	}
	latest, err := loadConversation(s.dataDir, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.RolledBackTurns) != 0 {
		t.Fatalf("no turn may be marked rolled back on a refused restore: %v", latest.RolledBackTurns)
	}
}
