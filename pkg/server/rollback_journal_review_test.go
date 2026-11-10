package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func fileModePerm(t *testing.T, work, rel string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(work, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("stat %s: %v", rel, err)
	}
	return info.Mode().Perm()
}

func journalAppliedFlags(t *testing.T, data, id string) []bool {
	t.Helper()
	j, err := loadRollbackJournal(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("expected journal")
	}
	out := make([]bool, len(j.Ops))
	for i, op := range j.Ops {
		out[i] = op.Applied
	}
	return out
}

// Crash window: first file has been renamed to the rollback target, but
// Applied was never persisted. Restart must compensate from backups, not
// treat the journal as a clean no-op and delete them.
func TestRollbackJournal_RestartCompensatesUnrecordedApply(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, map[string]string{"keep.txt": "keep\n"})

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
	// Leave Applied unset and persist that state. This is the crash
	// between a successful write and its journal mark.
	j.Phase = rollbackPhaseApplying
	if err := saveRollbackJournal(data, j); err != nil {
		t.Fatal(err)
	}
	flags := journalAppliedFlags(t, data, id)
	if len(flags) != 2 || flags[0] || flags[1] {
		t.Fatalf("fixture must persist Applied=false, got %v", flags)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" {
		t.Fatal("precondition: a.txt should already be at the rollback target")
	}

	ts := apiServer(t, work, data)
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if mustRead(t, work, "a.txt") != "A-new\n" {
		t.Fatalf("unrecorded apply must be compensated on restart, a=%q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("unapplied file should stay post-turn, b=%q", mustRead(t, work, "b.txt"))
	}
	if mustRead(t, work, "keep.txt") != "keep\n" {
		t.Fatal("unrelated file touched")
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("partial unrecorded apply must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared only after successful compensation")
	}
	if got["pending_rollback"] != nil {
		t.Fatalf("pending_rollback should be gone after compensate: %v", got["pending_rollback"])
	}
}

// External edit between plan validation and backup must not become the
// new BeforeHash and must not be overwritten.
func TestRollbackJournal_ExternalEditBeforePrepareIsNotBaselined(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)

	t.Cleanup(func() { rollbackBeforePrepareHook = nil; rollbackApplyHook = nil; rollbackCompensateHook = nil })
	rollbackBeforePrepareHook = func() {
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("external-after-plan\n"), 0644); err != nil {
			t.Errorf("inject external edit: %v", err)
		}
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("expected conflict, got %d %v", st, resp)
	}
	if mustRead(t, work, "a.txt") != "external-after-plan\n" {
		t.Fatalf("external edit was overwritten or re-baselined: %q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("unrelated/unstarted file changed: %q", mustRead(t, work, "b.txt"))
	}
	if journalExists(data, id) {
		t.Fatal("prepare must not leave a journal/backup baseline built from the external edit")
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
}

func TestRollbackJournal_PreservesExecutableModeOnApplyAndCompensate(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	script := filepath.Join(work, "run.sh")
	notes := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho new\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("new-notes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "run.sh", status: "modified", old: "#!/bin/sh\necho old\n", new: "#!/bin/sh\necho new\n"},
		{path: "notes.txt", status: "modified", old: "old-notes\n", new: "new-notes\n"},
	}, nil)
	if err := os.Chmod(script, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(notes, 0644); err != nil {
		t.Fatal(err)
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("rollback status=%d %v", st, resp)
	}
	if mustRead(t, work, "run.sh") != "#!/bin/sh\necho old\n" {
		t.Fatalf("script content: %q", mustRead(t, work, "run.sh"))
	}
	if fileModePerm(t, work, "run.sh") != 0755 {
		t.Fatalf("executable mode lost on rollback: %o", fileModePerm(t, work, "run.sh"))
	}
	if fileModePerm(t, work, "notes.txt") != 0644 {
		t.Fatalf("regular file mode changed on rollback: %o", fileModePerm(t, work, "notes.txt"))
	}

	// Recreate a mid-write failure and confirm compensate restores mode too.
	work2, data2 := t.TempDir(), t.TempDir()
	id2, sid2 := seedRollbackFixture(t, work2, data2, []seedFile{
		{path: "run.sh", status: "modified", old: "#!/bin/sh\necho old\n", new: "#!/bin/sh\necho new\n"},
		{path: "notes.txt", status: "modified", old: "old-notes\n", new: "new-notes\n"},
	}, nil)
	if err := os.WriteFile(filepath.Join(work2, "run.sh"), []byte("#!/bin/sh\necho new\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work2, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work2, "notes.txt"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
	rollbackApplyHook = func(path string) error {
		if path == "notes.txt" {
			return errRollbackInjected("injected write failure")
		}
		return nil
	}
	ts2 := apiServer(t, work2, data2)
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+id2+"/turns/"+sid2+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("expected compensated failure, got %d %v", st, resp)
	}
	if mustRead(t, work2, "run.sh") != "#!/bin/sh\necho new\n" {
		t.Fatalf("script should be compensated back, got %q", mustRead(t, work2, "run.sh"))
	}
	if fileModePerm(t, work2, "run.sh") != 0755 {
		t.Fatalf("executable mode lost on compensate: %o", fileModePerm(t, work2, "run.sh"))
	}
	if fileModePerm(t, work2, "notes.txt") != 0644 {
		t.Fatalf("unapplied notes mode changed: %o", fileModePerm(t, work2, "notes.txt"))
	}
}

func TestRollbackJournal_PendingBlocksSiblingConversationAndSessionResume(t *testing.T) {
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
		t.Fatalf("expected pending, got %d %v", st, resp)
	}
	if !journalExists(data, id) {
		t.Fatal("expected pending journal")
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "auto",
		"message": "start another conversation in the same workdir",
	})
	if st != http.StatusConflict {
		t.Fatalf("sibling conversation start want 409, got %d %v", st, resp)
	}
	sib, _ := resp["conversation"].(map[string]any)
	if sib == nil || sib["pending_rollback"] == nil {
		t.Fatalf("sibling start should surface workdir pending_rollback: %v", resp)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" {
		t.Fatalf("sibling run must not have written over the partial rollback: %q", mustRead(t, work, "a.txt"))
	}

	// Persist a raw session whose workdir is the pending directory, then
	// resume it through the session API (not the conversation that owns the journal).
	sessID := "sess_rbpending01"
	stStore, err := engine.NewStorage(data)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.NewTaskTree(sessID, "root", "resume after pending")
	tree.WorkDir = work
	if err := stStore.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sessID+"/resume?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("session resume want 409, got %d %v", st, resp)
	}

	// Barrier must still hold after process restart (New recovers, compensate still fails).
	ts2 := apiServer(t, work, data)
	if !journalExists(data, id) {
		t.Fatal("journal should remain after restart while compensate is still failing")
	}
	sibID := anyString(sib["id"])
	if sibID == "" {
		t.Fatal("missing sibling conversation id")
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+sibID+"/messages?token=test-token", map[string]any{
		"text": "try again after restart", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusConflict {
		t.Fatalf("post-restart sibling start want 409, got %d %v", st, resp)
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/sessions/"+sessID+"/resume?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("post-restart session resume want 409, got %d %v", st, resp)
	}
}

func TestRollbackJournal_UnreadableJournalFailClosed(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
	}, nil)
	_ = sid

	root := rollbackJournalRoot(data, id)
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackJournalPath(data, id), []byte("this is not valid json {"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := apiServer(t, work, data)
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	pend, _ := got["pending_rollback"].(map[string]any)
	if pend == nil || !strings.Contains(anyString(pend["message"]), "不是文件不存在") {
		t.Fatalf("GET should surface unreadable journal, not treat as absent: %v", got)
	}

	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write more", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusConflict {
		t.Fatalf("start with unreadable journal want 409, got %d %v", st, resp)
	}
	if !strings.Contains(anyString(resp["error"]), "不是文件不存在") {
		t.Fatalf("error should distinguish read failure from missing: %v", resp)
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "auto",
		"message": "sibling start",
	})
	if st != http.StatusConflict {
		t.Fatalf("sibling start want 409, got %d %v", st, resp)
	}

	sessID := "sess_rbunreadable01"
	stStore, err := engine.NewStorage(data)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.NewTaskTree(sessID, "root", "resume")
	tree.WorkDir = work
	if err := stStore.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sessID+"/resume?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("session resume want 409, got %d %v", st, resp)
	}

	st, rec := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/rollback/recover?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("recover unreadable want 409, got %d %v", st, rec)
	}
	if rec["pending"] != true {
		t.Fatalf("recover should stay blocked/pending: %v", rec)
	}

	// A missing journal (not a read failure) must still allow writes.
	work2, data2 := t.TempDir(), t.TempDir()
	id2, sid2 := seedRollbackFixture(t, work2, data2, []seedFile{
		{path: "ok.txt", status: "modified", old: "old\n", new: "new\n"},
	}, nil)
	_ = sid2
	ts2 := apiServerWithClient(t, work2, data2, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "ok"}), nil
		}}
	})
	if journalExists(data2, id2) {
		t.Fatal("control fixture should have no journal")
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+id2+"/messages?token=test-token", map[string]any{
		"text": "follow up without a journal", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusAccepted {
		t.Fatalf("missing journal should not block start, got %d %v", st, resp)
	}
	waitConvDone(t, ts2, id2)
}

func TestRollbackJournal_DeletedFileModeRestoredAndLegacyDefault(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "run.sh"), []byte("#!/bin/sh\necho keep\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes.txt"), []byte("keep-notes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work, "notes.txt"), 0644); err != nil {
		t.Fatal(err)
	}

	var step int
	actions := []struct {
		name string
		args map[string]any
	}{
		{tools.ToolDeletePath, map[string]any{"path": "run.sh"}},
		{tools.ToolDeletePath, map[string]any{"path": "notes.txt"}},
		{tools.ToolFinish, map[string]any{"summary": "deleted both"}},
	}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			if step >= len(actions) {
				return jsonAct(tools.ToolFinish, map[string]any{"summary": "done"}), nil
			}
			a := actions[step]
			step++
			return jsonAct(a.name, a.args), nil
		}}
	})
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "auto",
		"message": "delete the files",
	})
	if st != http.StatusCreated {
		t.Fatalf("create status=%d %v", st, resp)
	}
	id := anyString(resp["id"])
	done := waitConvDone(t, ts, id)
	sid := anyString(done["current_session_id"])
	if fileExists(work, "run.sh") || fileExists(work, "notes.txt") {
		t.Fatal("delete did not land")
	}
	changes, err := loadTurnChanges(data, id, sid)
	if err != nil {
		t.Fatal(err)
	}
	var sawExec, sawNotes bool
	for _, ch := range changes {
		if ch.Path == "run.sh" {
			sawExec = true
			if ch.OldMode != 0755 {
				t.Fatalf("snapshot should persist executable old_mode, got %o", ch.OldMode)
			}
		}
		if ch.Path == "notes.txt" {
			sawNotes = true
			if ch.OldMode != 0644 {
				t.Fatalf("snapshot should persist regular old_mode, got %o", ch.OldMode)
			}
		}
	}
	if !sawExec || !sawNotes {
		t.Fatalf("missing deleted changes: %+v", changes)
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("rollback status=%d %v", st, resp)
	}
	if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" {
		t.Fatalf("script content: %q", mustRead(t, work, "run.sh"))
	}
	if fileModePerm(t, work, "run.sh") != 0755 {
		t.Fatalf("deleted executable restored with wrong mode: %o", fileModePerm(t, work, "run.sh"))
	}
	if fileModePerm(t, work, "notes.txt") != 0644 {
		t.Fatalf("deleted regular file restored with wrong mode: %o", fileModePerm(t, work, "notes.txt"))
	}

	// Restart after a fully written restore that has not been committed:
	// files exist again with snapshot modes and stay that way after New().
	work2, data2 := t.TempDir(), t.TempDir()
	id2, sid2 := seedRollbackFixture(t, work2, data2, []seedFile{
		{path: "gone.sh", status: "deleted", old: "#!/bin/sh\necho x\n", new: ""},
	}, nil)
	// seed leaves a deleted file absent; record old mode as a fresh snapshot would.
	changes2, err := loadTurnChanges(data2, id2, sid2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range changes2 {
		if changes2[i].Path == "gone.sh" {
			changes2[i].OldMode = 0755
		}
	}
	if err := saveTurnChanges(data2, id2, sid2, changes2, nil); err != nil {
		t.Fatal(err)
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(apiServer(t, work2, data2))+"/conversations/"+id2+"/turns/"+sid2+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("seeded delete rollback: %d %v", st, resp)
	}
	if fileModePerm(t, work2, "gone.sh") != 0755 {
		t.Fatalf("restart-path delete restore mode: %o", fileModePerm(t, work2, "gone.sh"))
	}

	// Legacy snapshot without old_mode: restore content with default 0644,
	// and do not claim the original executable bit was recovered.
	work3, data3 := t.TempDir(), t.TempDir()
	id3, sid3 := seedRollbackFixture(t, work3, data3, []seedFile{
		{path: "legacy.sh", status: "deleted", old: "#!/bin/sh\necho legacy\n", new: ""},
	}, nil)
	st, resp = apiDo(t, http.MethodPost, apiBase(apiServer(t, work3, data3))+"/conversations/"+id3+"/turns/"+sid3+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("legacy rollback: %d %v", st, resp)
	}
	if mustRead(t, work3, "legacy.sh") != "#!/bin/sh\necho legacy\n" {
		t.Fatalf("legacy content: %q", mustRead(t, work3, "legacy.sh"))
	}
	if fileModePerm(t, work3, "legacy.sh") != 0644 {
		t.Fatalf("legacy snapshot without old_mode must default to 0644, got %o", fileModePerm(t, work3, "legacy.sh"))
	}
}

func TestRollbackJournal_RecoverClearsStaleConflicts(t *testing.T) {
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
	j.Phase = rollbackPhaseFailed
	j.Conflicts = []string{"stale.txt"}
	j.Error = "检测到外部修改（stale.txt），未覆盖这些文件。"
	if err := saveRollbackJournal(data, j); err != nil {
		t.Fatal(err)
	}

	ts := apiServer(t, work, data)
	st, rec := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/rollback/recover?token=test-token", map[string]any{})
	if st != http.StatusOK {
		t.Fatalf("recover should succeed after stale conflict is gone, got %d %v", st, rec)
	}
	if rec["pending"] == true {
		t.Fatalf("should not stay pending due to stale conflicts: %v", rec)
	}
	if conflicts, _ := rec["conflicts"].([]any); len(conflicts) > 0 {
		t.Fatalf("successful recover must not report stale conflicts: %v", rec)
	}
	if mustRead(t, work, "a.txt") != "A-new\n" || mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("compensate content wrong: a=%q b=%q", mustRead(t, work, "a.txt"), mustRead(t, work, "b.txt"))
	}
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if got["pending_rollback"] != nil {
		t.Fatalf("GET still shows pending/conflicts: %v", got["pending_rollback"])
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("compensated recover must not mark rolled_back: %v", c.RolledBackTurns)
	}
}

func seedSiblingRollbackConv(t *testing.T, work, data, convID, sid string, files []seedFile) {
	t.Helper()
	now := time.Now().UTC()
	c := &Conversation{
		ID:        convID,
		Title:     "sibling rollback",
		WorkDir:   work,
		CreatedAt: now,
		UpdatedAt: now,
		Settings: ConversationSettings{
			Model: "scripted", BaseURL: "http://127.0.0.1:9/v1",
			AgentMode: "direct", ApprovalMode: "auto",
		},
		SessionIDs:       []string{sid},
		CurrentSessionID: sid,
	}
	c.appendMessage("user", kindUser, "edit sibling files", sid, "", nil)
	c.appendMessage("system", kindRunFinished, "updated files", sid, "completed", nil)
	var changes []turnChange
	for _, f := range files {
		dest := filepath.Join(work, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(f.new), 0644); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, turnChange{Path: f.path, Status: f.status, Old: f.old, New: f.new})
	}
	if err := saveConversation(data, c, nil); err != nil {
		t.Fatal(err)
	}
	if err := saveTurnChanges(data, convID, sid, changes, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackJournal_SiblingRollbackBlockedByPendingOrUnreadable(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, nil)
	seedSiblingRollbackConv(t, work, data, "conv_rbsibling02", "sess_rbsib2", []seedFile{
		{path: "c.txt", status: "modified", old: "C-old\n", new: "C-new\n"},
	})

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
		t.Fatalf("owner pending, got %d %v", st, resp)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" || mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("partial owner rollback not in expected pending state")
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/conv_rbsibling02/turns/sess_rbsib2/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("sibling rollback with pending journal want 409, got %d %v", st, resp)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" || mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("sibling rollback overwrote pending recovery files")
	}
	if mustRead(t, work, "c.txt") != "C-new\n" {
		t.Fatalf("sibling rollback must not rewrite its own file: %q", mustRead(t, work, "c.txt"))
	}

	// Unreadable sibling journal (this conversation has no journal of its own).
	work2, data2 := t.TempDir(), t.TempDir()
	id2, _ := seedRollbackFixture(t, work2, data2, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
	}, nil)
	seedSiblingRollbackConv(t, work2, data2, "conv_rbsibling03", "sess_rbsib3", []seedFile{
		{path: "c.txt", status: "modified", old: "C-old\n", new: "C-new\n"},
	})
	if err := os.MkdirAll(rollbackJournalRoot(data2, id2), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackJournalPath(data2, id2), []byte("this is not valid json {"), 0644); err != nil {
		t.Fatal(err)
	}
	ts2 := apiServer(t, work2, data2)
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/conv_rbsibling03/turns/sess_rbsib3/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("sibling rollback with unreadable journal want 409, got %d %v", st, resp)
	}
	if !strings.Contains(anyString(resp["error"]), "不是文件不存在") {
		t.Fatalf("should distinguish read failure: %v", resp)
	}
	if mustRead(t, work2, "c.txt") != "C-new\n" || mustRead(t, work2, "a.txt") != "A-new\n" {
		t.Fatalf("unreadable pending must not be overwritten by sibling rollback")
	}
}

func TestRollbackJournal_IncompleteSchemaFailClosed(t *testing.T) {
	writeRaw := func(t *testing.T, data, convID string, body any) {
		t.Helper()
		if err := os.MkdirAll(rollbackJournalRoot(data, convID), 0755); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rollbackJournalPath(data, convID), raw, 0644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "missing workdir",
			body: map[string]any{
				"version": 1, "id": "rbj_incomplete1", "conv_id": "conv_rbjournal01",
				"session_id": "sess_rb1", "phase": "failed", "ops": []any{},
			},
			want: "missing workdir",
		},
		{
			name: "unknown version",
			body: map[string]any{
				"version": 99, "id": "rbj_incomplete2", "conv_id": "conv_rbjournal01",
				"session_id": "sess_rb1", "workdir": "/tmp/somewhere", "phase": "failed", "ops": []any{},
			},
			want: "unsupported rollback journal version",
		},
		{
			name: "unknown phase",
			body: map[string]any{
				"version": 1, "id": "rbj_incomplete3", "conv_id": "conv_rbjournal01",
				"session_id": "sess_rb1", "workdir": "/tmp/somewhere", "phase": "mystery", "ops": []any{},
			},
			want: "unknown phase",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work, data := t.TempDir(), t.TempDir()
			id, sid := seedRollbackFixture(t, work, data, []seedFile{
				{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
			}, nil)
			body := map[string]any{}
			for k, v := range tc.body {
				body[k] = v
			}
			body["conv_id"] = id
			if wd, ok := body["workdir"].(string); ok && wd == "/tmp/somewhere" {
				body["workdir"] = work
			}
			writeRaw(t, data, id, body)

			ts := apiServer(t, work, data)
			_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
			pend, _ := got["pending_rollback"].(map[string]any)
			if pend == nil {
				t.Fatalf("GET should not treat incomplete journal as absent: %v", got)
			}
			if !strings.Contains(anyString(pend["message"]), "不是文件不存在") {
				t.Fatalf("message should be a read/schema failure, got %v", pend)
			}

			st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
				"text": "write more", "api_key": testSecretKey, "mode": "start",
			})
			if st != http.StatusConflict {
				t.Fatalf("start want 409, got %d %v", st, resp)
			}
			if !strings.Contains(anyString(resp["error"]), tc.want) && !strings.Contains(anyString(resp["error"]), "不是文件不存在") {
				t.Fatalf("error should mention schema problem %q: %v", tc.want, resp)
			}

			st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
			if st != http.StatusConflict {
				t.Fatalf("rollback want 409, got %d %v", st, resp)
			}
			if mustRead(t, work, "a.txt") != "A-new\n" {
				t.Fatalf("incomplete journal must not be treated as no pending: %q", mustRead(t, work, "a.txt"))
			}

			// Sibling conversation in the same workdir is also blocked.
			seedSiblingRollbackConv(t, work, data, "conv_rbschema02", "sess_rbschema2", []seedFile{
				{path: "c.txt", status: "modified", old: "C-old\n", new: "C-new\n"},
			})
			st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/conv_rbschema02/turns/sess_rbschema2/rollback?token=test-token", map[string]any{"confirm": true})
			if st != http.StatusConflict {
				t.Fatalf("sibling rollback want 409, got %d %v", st, resp)
			}
			if mustRead(t, work, "c.txt") != "C-new\n" {
				t.Fatalf("sibling file was overwritten")
			}
		})
	}
}

func TestRollbackJournal_PermissionOnlyChangeIsReverted(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	script := filepath.Join(work, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0644); err != nil {
		t.Fatal(err)
	}

	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			if err := os.Chmod(script, 0755); err != nil {
				t.Errorf("chmod: %v", err)
			}
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "made executable"}), nil
		}}
	})
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "auto",
		"message": "make the script executable without changing contents",
	})
	if st != http.StatusCreated {
		t.Fatalf("create status=%d %v", st, resp)
	}
	id := anyString(resp["id"])
	done := waitConvDone(t, ts, id)
	sid := anyString(done["current_session_id"])
	if fileModePerm(t, work, "run.sh") != 0755 {
		t.Fatalf("turn should have left the executable bit, got %o", fileModePerm(t, work, "run.sh"))
	}
	if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" {
		t.Fatal("content should be unchanged")
	}
	changes, err := loadTurnChanges(data, id, sid)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, ch := range changes {
		if ch.Path != "run.sh" {
			continue
		}
		saw = true
		if ch.Status != "modified" || ch.OldMode != 0644 || ch.NewMode != 0755 {
			t.Fatalf("expected permission-only change, got %+v", ch)
		}
		if ch.Old != ch.New {
			t.Fatalf("content should be identical in the snapshot, old=%q new=%q", ch.Old, ch.New)
		}
	}
	if !saw {
		t.Fatalf("pure chmod was omitted from turn changes: %+v", changes)
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("rollback status=%d %v", st, resp)
	}
	if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" {
		t.Fatalf("content changed by rollback: %q", mustRead(t, work, "run.sh"))
	}
	if fileModePerm(t, work, "run.sh") != 0644 {
		t.Fatalf("permission-only rollback should restore 0644, got %o", fileModePerm(t, work, "run.sh"))
	}

	// Legacy snapshots with Mode 0 must not invent a permission-only change.
	before := []snapFile{{Rel: "legacy.sh", Size: 4, Hash: fileDigest(true, "data"), Content: "data", Mode: 0}}
	afterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(afterDir, "legacy.sh"), []byte("data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(afterDir, "legacy.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	// Recompute hash from the actual snapshot helper by faking after via workdir
	// and a before entry that matches hash/size once we snapshot.
	// Use diffAgainstSnapshot with a before snapshot whose hash matches after.
	after, err := snapshotWorkdir(afterDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Mode != 0755 {
		t.Fatalf("after snapshot: %+v", after)
	}
	before[0].Hash = after[0].Hash
	before[0].Size = after[0].Size
	before[0].Content = after[0].Content
	legacy, err := diffAgainstSnapshot(before, afterDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range legacy {
		if ch.Path == "legacy.sh" {
			t.Fatalf("legacy snapshot without mode must not record a permission-only change: %+v", ch)
		}
	}
}

func seedMixedChmodAndContent(t *testing.T, work, data string) (convID, sid string) {
	t.Helper()
	script := "#!/bin/sh\necho keep\n"
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "run.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "data.txt"), []byte("D-new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(work, "data.txt"), 0644); err != nil {
		t.Fatal(err)
	}
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "run.sh", status: "modified", old: script, new: script},
		{path: "data.txt", status: "modified", old: "D-old\n", new: "D-new\n"},
	}, nil)
	if err := os.Chmod(filepath.Join(work, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	changes, err := loadTurnChanges(data, id, sid)
	if err != nil {
		t.Fatal(err)
	}
	for i := range changes {
		switch changes[i].Path {
		case "run.sh":
			changes[i].OldMode = 0644
			changes[i].NewMode = 0755
		case "data.txt":
			changes[i].OldMode = 0644
			changes[i].NewMode = 0644
		}
	}
	if err := saveTurnChanges(data, id, sid, changes, nil); err != nil {
		t.Fatal(err)
	}
	return id, sid
}

func seedAllChmodFixture(t *testing.T, work, data string) (convID, sid string) {
	t.Helper()
	files := []struct{ path, body string }{
		{"run.sh", "#!/bin/sh\necho a\n"},
		{"tool.sh", "#!/bin/sh\necho b\n"},
	}
	var seeds []seedFile
	for _, f := range files {
		if err := os.MkdirAll(work, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, f.path), []byte(f.body), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(work, f.path), 0755); err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, seedFile{path: f.path, status: "modified", old: f.body, new: f.body})
	}
	id, sid := seedRollbackFixture(t, work, data, seeds, nil)
	for _, f := range files {
		if err := os.Chmod(filepath.Join(work, f.path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := loadTurnChanges(data, id, sid)
	if err != nil {
		t.Fatal(err)
	}
	for i := range changes {
		changes[i].OldMode = 0644
		changes[i].NewMode = 0755
	}
	if err := saveTurnChanges(data, id, sid, changes, nil); err != nil {
		t.Fatal(err)
	}
	return id, sid
}

func TestRollbackJournal_MixedChmodContentCompensateAndRestart(t *testing.T) {
	t.Run("midWriteFailureRestoresChmodMode", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedMixedChmodAndContent(t, work, data)
		t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
		rollbackApplyHook = func(path string) error {
			if path == "data.txt" {
				return errRollbackInjected("injected write failure")
			}
			return nil
		}
		ts := apiServer(t, work, data)
		st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
		if st != http.StatusConflict {
			t.Fatalf("expected compensated failure, got %d %v", st, resp)
		}
		if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" {
			t.Fatalf("chmod file content changed: %q", mustRead(t, work, "run.sh"))
		}
		if fileModePerm(t, work, "run.sh") != 0755 {
			t.Fatalf("pure chmod file must be compensated back to post-turn 0755, got %o", fileModePerm(t, work, "run.sh"))
		}
		if mustRead(t, work, "data.txt") != "D-new\n" {
			t.Fatalf("unapplied content file should stay post-turn: %q", mustRead(t, work, "data.txt"))
		}
		if fileModePerm(t, work, "data.txt") != 0644 {
			t.Fatalf("unapplied content file mode: %o", fileModePerm(t, work, "data.txt"))
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if containsString(c.RolledBackTurns, sid) {
			t.Fatalf("compensated failure must not mark rolled_back: %v", c.RolledBackTurns)
		}
		if journalExists(data, id) {
			t.Fatal("journal should be cleared after successful compensation")
		}
	})

	t.Run("unrecordedChmodWriteIsCompensatedOnRestart", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedMixedChmodAndContent(t, work, data)
		j, err := prepareRollbackJournal(data, id, sid, work, []rollbackOp{
			{path: "run.sh", restore: "#!/bin/sh\necho keep\n", restoreMode: 0644},
			{path: "data.txt", restore: "D-old\n", restoreMode: 0644},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicWriteWorkdirFileMode(work, "run.sh", "#!/bin/sh\necho keep\n", 0644); err != nil {
			t.Fatal(err)
		}
		// Leave Applied unset: file is at the restore target, journal is not.
		if j.Ops[0].Applied || j.Ops[1].Applied {
			t.Fatal("fixture must leave Applied unset")
		}
		if err := saveRollbackJournal(data, j); err != nil {
			t.Fatal(err)
		}
		if fileModePerm(t, work, "run.sh") != 0644 {
			t.Fatalf("setup: unrecorded chmod write: %o", fileModePerm(t, work, "run.sh"))
		}

		ts := apiServer(t, work, data)
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if fileModePerm(t, work, "run.sh") != 0755 {
			t.Fatalf("restart must compensate unrecorded chmod back to 0755, got %o", fileModePerm(t, work, "run.sh"))
		}
		if mustRead(t, work, "data.txt") != "D-new\n" {
			t.Fatalf("unapplied content file changed: %q", mustRead(t, work, "data.txt"))
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if containsString(c.RolledBackTurns, sid) {
			t.Fatalf("partial unrecorded chmod must not be committed as rolled_back: %v", c.RolledBackTurns)
		}
		if journalExists(data, id) {
			t.Fatal("journal should be cleared after successful compensation")
		}
		if got["pending_rollback"] != nil {
			t.Fatalf("pending after compensate: %v", got["pending_rollback"])
		}
	})

	t.Run("allChmodUnappliedRestartDoesNotCommit", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedAllChmodFixture(t, work, data)
		if _, err := prepareRollbackJournal(data, id, sid, work, []rollbackOp{
			{path: "run.sh", restore: "#!/bin/sh\necho a\n", restoreMode: 0644},
			{path: "tool.sh", restore: "#!/bin/sh\necho b\n", restoreMode: 0644},
		}); err != nil {
			t.Fatal(err)
		}
		// Nothing applied: files still have post-turn 0755, but content already
		// matches the target hash. Restart must not treat this as complete.
		if fileModePerm(t, work, "run.sh") != 0755 || fileModePerm(t, work, "tool.sh") != 0755 {
			t.Fatal("setup should leave post-turn modes")
		}
		ts := apiServer(t, work, data)
		_, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if fileModePerm(t, work, "run.sh") != 0755 || fileModePerm(t, work, "tool.sh") != 0755 {
			t.Fatalf("unapplied chmod journal must not change modes on restart: run=%o tool=%o", fileModePerm(t, work, "run.sh"), fileModePerm(t, work, "tool.sh"))
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if containsString(c.RolledBackTurns, sid) {
			t.Fatalf("content-only match must not mark rolled_back when modes are still post-turn: %v", c.RolledBackTurns)
		}
	})

	t.Run("allChmodUnrecordedFullApplyCommitsOnRestart", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedAllChmodFixture(t, work, data)
		j, err := prepareRollbackJournal(data, id, sid, work, []rollbackOp{
			{path: "run.sh", restore: "#!/bin/sh\necho a\n", restoreMode: 0644},
			{path: "tool.sh", restore: "#!/bin/sh\necho b\n", restoreMode: 0644},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicWriteWorkdirFileMode(work, "run.sh", "#!/bin/sh\necho a\n", 0644); err != nil {
			t.Fatal(err)
		}
		if err := atomicWriteWorkdirFileMode(work, "tool.sh", "#!/bin/sh\necho b\n", 0644); err != nil {
			t.Fatal(err)
		}
		if err := saveRollbackJournal(data, j); err != nil {
			t.Fatal(err)
		}
		ts := apiServer(t, work, data)
		_, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if fileModePerm(t, work, "run.sh") != 0644 || fileModePerm(t, work, "tool.sh") != 0644 {
			t.Fatalf("fully restored chmod modes should be kept: run=%o tool=%o", fileModePerm(t, work, "run.sh"), fileModePerm(t, work, "tool.sh"))
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if !containsString(c.RolledBackTurns, sid) {
			t.Fatalf("unrecorded but fully restored chmod journal should commit rolled_back: %v", c.RolledBackTurns)
		}
		if journalExists(data, id) {
			t.Fatal("journal should be cleared after commit")
		}
	})
}

func TestRollbackJournal_ExternalModeChangeIsNotOverwritten(t *testing.T) {
	t.Run("preWriteExternalChmodIsConflict", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedMixedChmodAndContent(t, work, data)
		t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
		rollbackApplyHook = func(path string) error {
			if path == "run.sh" {
				if err := os.Chmod(filepath.Join(work, "run.sh"), 0700); err != nil {
					return err
				}
			}
			return nil
		}
		ts := apiServer(t, work, data)
		st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
		if st != http.StatusConflict {
			t.Fatalf("expected conflict after external chmod, got %d %v", st, resp)
		}
		if fileModePerm(t, work, "run.sh") != 0700 {
			t.Fatalf("external chmod of unapplied file was overwritten: %o", fileModePerm(t, work, "run.sh"))
		}
		if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" {
			t.Fatalf("content: %q", mustRead(t, work, "run.sh"))
		}
		if mustRead(t, work, "data.txt") != "D-new\n" {
			t.Fatalf("later file should not be written: %q", mustRead(t, work, "data.txt"))
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if containsString(c.RolledBackTurns, sid) {
			t.Fatalf("must not mark rolled_back: %v", c.RolledBackTurns)
		}
	})

	t.Run("compensateSkipsExternallyChmodAppliedFile", func(t *testing.T) {
		work, data := t.TempDir(), t.TempDir()
		id, sid := seedMixedChmodAndContent(t, work, data)
		t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
		rollbackApplyHook = func(path string) error {
			if path == "data.txt" {
				if err := os.Chmod(filepath.Join(work, "run.sh"), 0700); err != nil {
					return err
				}
				return errRollbackInjected("injected write failure")
			}
			return nil
		}
		ts := apiServer(t, work, data)
		st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
		if st != http.StatusConflict {
			t.Fatalf("expected conflict, got %d %v", st, resp)
		}
		if fileModePerm(t, work, "run.sh") != 0700 {
			t.Fatalf("already-applied file with external chmod must be left alone, got %o", fileModePerm(t, work, "run.sh"))
		}
		if mustRead(t, work, "run.sh") != "#!/bin/sh\necho keep\n" || mustRead(t, work, "data.txt") != "D-new\n" {
			t.Fatalf("content should stay (run already restored, data unapplied)")
		}
		c, err := loadConversation(data, id)
		if err != nil {
			t.Fatal(err)
		}
		if containsString(c.RolledBackTurns, sid) {
			t.Fatalf("must not mark rolled_back: %v", c.RolledBackTurns)
		}
		if !journalExists(data, id) {
			t.Fatal("conflict during compensate must keep the journal")
		}
	})
}

func TestJournalOpStateMatchLegacyUnknownMode(t *testing.T) {
	op := rollbackJournalOp{BeforeHash: fileDigest(true, "same"), TargetHash: fileDigest(true, "same"), BeforeMode: 0, RestoreMode: 0}
	if !journalOpMatchesBefore(op, true, op.BeforeHash, 0755) || !journalOpMatchesTarget(op, true, op.TargetHash, 0) {
		t.Fatal("unknown mode 0 (including a real 0000 sentinel) must fall back to content-only comparison")
	}
	op.BeforeMode = 0644
	op.RestoreMode = 0755
	if journalOpMatchesBefore(op, true, op.BeforeHash, 0755) {
		t.Fatal("known before mode must not match a different current mode")
	}
	if journalOpMatchesTarget(op, true, op.TargetHash, 0644) {
		t.Fatal("known restore mode must not match a different current mode")
	}
	if !journalOpMatchesBefore(op, true, op.BeforeHash, 0644) || !journalOpMatchesTarget(op, true, op.TargetHash, 0755) {
		t.Fatal("known modes should match when bits and content agree")
	}
}
