package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

type seedFile struct {
	path, status, old, new string
}

func seedRollbackFixture(t *testing.T, work, data string, files []seedFile, extras map[string]string) (convID, sid string) {
	t.Helper()
	convID = "conv_rbjournal01"
	sid = "sess_rb1"
	now := time.Now().UTC()
	c := &Conversation{
		ID:        convID,
		Title:     "rollback journal",
		WorkDir:   work,
		CreatedAt: now,
		UpdatedAt: now,
		Settings: ConversationSettings{
			Model:        "scripted",
			BaseURL:      "http://127.0.0.1:9/v1",
			AgentMode:    "direct",
			ApprovalMode: "auto",
		},
		SessionIDs:       []string{sid},
		CurrentSessionID: sid,
	}
	c.appendMessage("user", kindUser, "edit the files", sid, "", nil)
	c.appendMessage("system", kindRunFinished, "updated files", sid, "completed", nil)
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	var changes []turnChange
	for _, f := range files {
		dest := filepath.Join(work, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		switch f.status {
		case "deleted":
			_ = os.Remove(dest)
		default:
			if err := os.WriteFile(dest, []byte(f.new), 0644); err != nil {
				t.Fatal(err)
			}
		}
		changes = append(changes, turnChange{
			Path: f.path, Status: f.status, Old: f.old, New: f.new,
		})
	}
	for rel, body := range extras {
		dest := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	if err := saveTurnChanges(data, convID, sid, changes, nil); err != nil {
		t.Fatal(err)
	}
	return convID, sid
}

func mustRead(t *testing.T, work, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func fileExists(work, rel string) bool {
	_, err := os.Stat(filepath.Join(work, filepath.FromSlash(rel)))
	return err == nil
}

func journalExists(data, id string) bool {
	_, err := os.Stat(rollbackJournalPath(data, id))
	return err == nil
}

func TestRollbackJournal_SuccessAddDeleteEmptyAndMemory(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "mod.txt", status: "modified", old: "old-mod\n", new: "new-mod\n"},
		{path: "empty.txt", status: "modified", old: "was-text\n", new: ""},
		{path: "added.txt", status: "added", old: "", new: "brand-new\n"},
		{path: "added-empty.txt", status: "added", old: "", new: ""},
		{path: "gone.txt", status: "deleted", old: "restore-me\n", new: ""},
	}, map[string]string{"keep.txt": "untouched\n"})

	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "noted"}), nil
		}}
	})

	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("rollback status=%d resp=%v", st, resp)
	}
	if mustRead(t, work, "mod.txt") != "old-mod\n" {
		t.Fatalf("mod.txt not restored: %q", mustRead(t, work, "mod.txt"))
	}
	if mustRead(t, work, "empty.txt") != "was-text\n" {
		t.Fatalf("empty-file edit was treated as delete: %q", mustRead(t, work, "empty.txt"))
	}
	if fileExists(work, "added.txt") || fileExists(work, "added-empty.txt") {
		t.Fatal("added files should have been deleted")
	}
	if mustRead(t, work, "gone.txt") != "restore-me\n" {
		t.Fatalf("deleted file not restored: %q", mustRead(t, work, "gone.txt"))
	}
	if mustRead(t, work, "keep.txt") != "untouched\n" {
		t.Fatalf("unrelated file touched: %q", mustRead(t, work, "keep.txt"))
	}
	if journalExists(data, id) {
		t.Fatal("journal should be removed after success")
	}
	st, ch := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("changes status=%d %v", st, ch)
	}
	if ch["editable"] == true {
		t.Fatalf("rolled-back turn must not stay hunk-editable: %v", ch)
	}
	for _, item := range ch["changes"].([]any) {
		cm := item.(map[string]any)
		if anyString(cm["applied_status"]) != "rolled_back" {
			t.Fatalf("change %v should be rolled_back", cm)
		}
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(c.RolledBackTurns, sid) {
		t.Fatalf("rolled_back_turns missing %s: %v", sid, c.RolledBackTurns)
	}
	mem := conversationMemory(c)
	if !strings.Contains(mem, "ROLLED BACK") && !strings.Contains(mem, "rolled back") {
		t.Fatalf("conversation memory must record the rollback: %s", mem)
	}

	var mu sync.Mutex
	var follow string
	ts2 := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			mu.Lock()
			follow += joinMsgs(req)
			mu.Unlock()
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "understood the rollback"}), nil
		}}
	})
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "what is on disk now?", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusAccepted {
		t.Fatalf("follow-up status=%d resp=%v", st, resp)
	}
	done := waitConvDoneAfter(t, ts2, id, sid)
	_ = done
	mu.Lock()
	got := follow
	mu.Unlock()
	if !strings.Contains(got, "ROLLED BACK") && !strings.Contains(got, "rolled back") && !strings.Contains(strings.ToLower(got), "no longer") {
		t.Fatalf("follow-up model context missing rollback note: %s", got)
	}
}

func TestRollbackJournal_MidWriteFailureCompensates(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, map[string]string{"keep.txt": "keep\n"})

	t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
	rollbackApplyHook = func(path string) error {
		if path == "b.txt" {
			return errRollbackInjected("injected write failure")
		}
		return nil
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("expected mid-write failure conflict, got status=%d resp=%v", st, resp)
	}
	if _, ok := resp["conversation"].(map[string]any); !ok {
		t.Fatalf("compensated failure must include conversation, not a bare error: %v", resp)
	}
	if mustRead(t, work, "a.txt") != "A-new\n" {
		t.Fatalf("a.txt should be compensated back to post-turn: %q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("b.txt should stay post-turn: %q", mustRead(t, work, "b.txt"))
	}
	if mustRead(t, work, "keep.txt") != "keep\n" {
		t.Fatalf("unrelated file touched: %q", mustRead(t, work, "keep.txt"))
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("must not mark rolled_back_turns after compensated failure: %v", c.RolledBackTurns)
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared after successful compensation")
	}
}

func TestRollbackJournal_CompensateFailureBlocksWritesAndRestart(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)

	t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
	rollbackApplyHook = func(path string) error {
		if path == "b.txt" {
			return errRollbackInjected("injected write failure")
		}
		return nil
	}
	rollbackCompensateHook = func(path string) error {
		return errRollbackInjected("injected compensate failure")
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("expected pending conflict (not 500-only), got %d %v", st, resp)
	}
	if st == http.StatusInternalServerError {
		t.Fatalf("pending rollback must not be a bare 500: %v", resp)
	}
	conv, _ := resp["conversation"].(map[string]any)
	if conv == nil {
		t.Fatalf("pending failure must include conversation: %v", resp)
	}
	if conv["pending_rollback"] == nil {
		t.Fatalf("pending_rollback missing on rollback response: %v", conv)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" {
		t.Fatalf("a.txt should remain at the restored value after compensate failed: %q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("b.txt should stay post-turn: %q", mustRead(t, work, "b.txt"))
	}
	if !journalExists(data, id) {
		t.Fatal("journal and backups must be kept after compensate failure")
	}
	if _, err := os.Stat(filepath.Join(rollbackBackupDir(data, id), "a.txt")); err != nil {
		t.Fatalf("backup for a.txt missing: %v", err)
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}

	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	pend, _ := got["pending_rollback"].(map[string]any)
	if pend == nil {
		t.Fatalf("GET missing pending_rollback: %v", got)
	}
	if !strings.Contains(anyString(pend["message"]), "未完成") {
		t.Fatalf("pending message unclear: %v", pend)
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "start another turn", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusConflict {
		t.Fatalf("start should be blocked, status=%d resp=%v", st, resp)
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "a.txt", "all": "accept",
	})
	if st != http.StatusConflict {
		t.Fatalf("hunk write should be blocked, status=%d resp=%v", st, resp)
	}

	// External mid-recovery edit of the unrestored file must not be overwritten
	// on restart, and the journal must remain.
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("external-after-partial\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rollbackApplyHook = nil
	rollbackCompensateHook = nil
	ts2 := apiServer(t, work, data)
	_, got = apiDo(t, http.MethodGet, apiBase(ts2)+"/conversations/"+id+"?token=test-token", nil)
	if mustRead(t, work, "a.txt") != "external-after-partial\n" {
		t.Fatalf("restart recover overwrote an external edit: %q", mustRead(t, work, "a.txt"))
	}
	if !journalExists(data, id) {
		t.Fatal("journal should remain when an external edit blocks compensation")
	}
	pend, _ = got["pending_rollback"].(map[string]any)
	if pend == nil {
		t.Fatalf("restart GET missing pending_rollback: %v", got)
	}
	c, err = loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("external-edit recover must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
}

func TestRollbackJournal_RestartCompletesFullyApplied(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)

	// Simulate a crash after both files were restored but before the
	// conversation was marked rolled back.
	s, err := New(agent.NewRunManager(), data, WithToken("test-token"), WithWorkdir(work))
	if err != nil {
		t.Fatal(err)
	}
	plan := []rollbackOp{
		{path: "a.txt", restore: "A-old\n"},
		{path: "b.txt", restore: "B-new\n"}, // placeholder, we'll apply ourselves
	}
	// Build a realistic journal from the post-turn workdir, then apply both
	// files and leave the journal in the applying/done-uncommitted state.
	_ = plan
	j, err := prepareRollbackJournal(data, id, sid, work, []rollbackOp{
		{path: "a.txt", restore: "A-old\n"},
		{path: "b.txt", restore: "B-old\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteWorkdirFile(work, "a.txt", "A-old\n"); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteWorkdirFile(work, "b.txt", "B-old\n"); err != nil {
		t.Fatal(err)
	}
	for i := range j.Ops {
		j.Ops[i].Applied = true
	}
	j.Phase = rollbackPhaseApplying
	if err := saveRollbackJournal(data, j); err != nil {
		t.Fatal(err)
	}
	_ = s

	ts := apiServer(t, work, data) // New() recovers pending journals
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if mustRead(t, work, "a.txt") != "A-old\n" || mustRead(t, work, "b.txt") != "B-old\n" {
		t.Fatalf("files not left restored: a=%q b=%q", mustRead(t, work, "a.txt"), mustRead(t, work, "b.txt"))
	}
	ids, _ := got["rolled_back_turns"].([]any)
	found := false
	for _, item := range ids {
		if anyString(item) == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("restart did not record rolled_back_turns: %v", got["rolled_back_turns"])
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared after restart completed the rollback")
	}
	if got["pending_rollback"] != nil {
		t.Fatalf("pending_rollback should be gone: %v", got["pending_rollback"])
	}
	mem := conversationMemoryMust(t, data, id)
	if !strings.Contains(mem, "ROLLED BACK") && !strings.Contains(mem, "rolled back") {
		t.Fatalf("memory missing rollback after restart complete: %s", mem)
	}
}

func TestRollbackJournal_RestartCompensatesPartialApply(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)

	j, err := prepareRollbackJournal(data, id, sid, work, []rollbackOp{
		{path: "a.txt", restore: "A-old\n"},
		{path: "b.txt", restore: "B-old\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteWorkdirFile(work, "a.txt", "A-old\n"); err != nil {
		t.Fatal(err)
	}
	j.Ops[0].Applied = true
	j.Phase = rollbackPhaseApplying
	if err := saveRollbackJournal(data, j); err != nil {
		t.Fatal(err)
	}

	ts := apiServer(t, work, data)
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if mustRead(t, work, "a.txt") != "A-new\n" {
		t.Fatalf("partial rollback should be compensated: a=%q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("unapplied file should stay post-turn: b=%q", mustRead(t, work, "b.txt"))
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared after successful restart compensation")
	}
	if got["pending_rollback"] != nil {
		t.Fatalf("pending_rollback should be gone after compensate: %v", got["pending_rollback"])
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("compensated restart must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
}

func TestRollbackJournal_MidApplyExternalEditIsNotOverwritten(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)

	t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
	rollbackApplyHook = func(path string) error {
		if path == "b.txt" {
			if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("external-b\n"), 0644); err != nil {
				return err
			}
		}
		return nil
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("expected conflict after external edit, got %d %v", st, resp)
	}
	if mustRead(t, work, "b.txt") != "external-b\n" {
		t.Fatalf("external edit of unapplied file was overwritten: %q", mustRead(t, work, "b.txt"))
	}
	if mustRead(t, work, "a.txt") != "A-new\n" {
		t.Fatalf("already-written file should be compensated: %q", mustRead(t, work, "a.txt"))
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
}

func conversationMemoryMust(t *testing.T, data, id string) string {
	t.Helper()
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	return conversationMemory(c)
}

type injectedError string

func (e injectedError) Error() string { return string(e) }

func errRollbackInjected(msg string) error { return injectedError(msg) }
