package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Independent temp-project acceptance of the journaled rollback loop
// through the public HTTP API, with a scripted (not remote) model.
func TestRollbackJournal_APIAcceptanceIndependentProject(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		dest := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("mod.txt", "old-mod\n")
	mustWrite("empty.txt", "was-text\n")
	mustWrite("gone.txt", "restore-me\n")
	mustWrite("keep.txt", "untouched\n")

	var step int
	actions := []struct {
		name string
		args map[string]any
	}{
		{tools.ToolWriteFile, map[string]any{"path": "mod.txt", "content": "new-mod\n"}},
		{tools.ToolWriteFile, map[string]any{"path": "empty.txt", "content": "cleared\n"}},
		{tools.ToolWriteFile, map[string]any{"path": "added.txt", "content": "brand-new\n"}},

		{tools.ToolDeletePath, map[string]any{"path": "gone.txt"}},
		{tools.ToolFinish, map[string]any{"summary": "updated the project files"}},
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
		"message": "update several files",
	})
	if st != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", st, resp)
	}
	id := anyString(resp["id"])
	done := waitConvDone(t, ts, id)
	sid := anyString(done["current_session_id"])
	if sid == "" {
		t.Fatal("missing session id after scripted turn")
	}
	if mustRead(t, work, "mod.txt") != "new-mod\n" || mustRead(t, work, "empty.txt") != "cleared\n" || !fileExists(work, "added.txt") || fileExists(work, "gone.txt") {
		t.Fatalf("scripted turn did not land expected files: mod=%q empty=%q added=%v gone=%v",
			mustRead(t, work, "mod.txt"), mustRead(t, work, "empty.txt"), fileExists(work, "added.txt"), fileExists(work, "gone.txt"))
	}

	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusOK {
		t.Fatalf("rollback status=%d resp=%v", st, resp)
	}
	if mustRead(t, work, "mod.txt") != "old-mod\n" {
		t.Fatalf("mod.txt: %q", mustRead(t, work, "mod.txt"))
	}
	if mustRead(t, work, "empty.txt") != "was-text\n" {
		t.Fatalf("empty.txt not restored: %q", mustRead(t, work, "empty.txt"))
	}
	if fileExists(work, "added.txt") {
		t.Fatal("added file should have been removed")
	}
	if mustRead(t, work, "gone.txt") != "restore-me\n" {
		t.Fatalf("gone.txt: %q", mustRead(t, work, "gone.txt"))
	}
	if mustRead(t, work, "keep.txt") != "untouched\n" {
		t.Fatal("unrelated file touched")
	}
	if journalExists(data, id) {
		t.Fatal("journal should be gone after success")
	}
	conv, _ := resp["conversation"].(map[string]any)
	if conv == nil {
		t.Fatal("success response missing conversation")
	}
	if conv["pending_rollback"] != nil {
		t.Fatalf("pending_rollback after success: %v", conv["pending_rollback"])
	}

	st, ch := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("changes %d %v", st, ch)
	}
	if ch["editable"] == true {
		t.Fatalf("hunk review must be closed after rollback: %v", ch)
	}
	for _, item := range ch["changes"].([]any) {
		cm := item.(map[string]any)
		if anyString(cm["applied_status"]) != "rolled_back" {
			t.Fatalf("expected rolled_back, got %v", cm)
		}
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "mod.txt", "all": "accept",
	})
	if st != http.StatusConflict {
		t.Fatalf("hunk apply after rollback should be blocked, status=%d %v", st, resp)
	}

	var mu sync.Mutex
	var follow string
	ts2 := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			mu.Lock()
			follow += joinMsgs(req)
			mu.Unlock()
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "noted rollback"}), nil
		}}
	})
	st, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "what is on disk now?", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusAccepted {
		t.Fatalf("follow-up status=%d %v", st, resp)
	}
	_ = waitConvDoneAfter(t, ts2, id, sid)
	mu.Lock()
	got := follow
	mu.Unlock()
	if !containsFold(got, "rolled back") && !containsFold(got, "no longer") {
		t.Fatalf("follow-up context missing rollback memory: %s", got)
	}
}

func TestRollbackJournal_PendingRecoverAndWriteBlocks(t *testing.T) {
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
	rollbackCompensateHook = func(path string) error {
		return errRollbackInjected("injected compensate failure")
	}

	ts := apiServer(t, work, data)
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
	if st != http.StatusConflict {
		t.Fatalf("pending rollback want 409, got %d %v", st, resp)
	}
	conv, _ := resp["conversation"].(map[string]any)
	if conv == nil || conv["pending_rollback"] == nil {
		t.Fatalf("UI-facing pending_rollback missing: %v", resp)
	}

	st, rec := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/rollback/recover?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("recover while compensate still failing want 409, got %d %v", st, rec)
	}
	if rec["pending"] != true {
		t.Fatalf("recover should stay pending: %v", rec)
	}
	if rec["conversation"] == nil {
		t.Fatalf("recover conflict must include conversation: %v", rec)
	}

	st, msg := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write more", "api_key": testSecretKey, "mode": "start",
	})
	if st != http.StatusConflict {
		t.Fatalf("start want 409, got %d %v", st, msg)
	}
	if msgConv, _ := msg["conversation"].(map[string]any); msgConv == nil || msgConv["pending_rollback"] == nil {
		t.Fatalf("blocked start must carry pending_rollback, not a bare error: %v", msg)
	}

	st, resu := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/resume?token=test-token", map[string]any{})
	if st != http.StatusConflict {
		t.Fatalf("resume want 409, got %d %v", st, resu)
	}
	if resuConv, _ := resu["conversation"].(map[string]any); resuConv == nil || resuConv["pending_rollback"] == nil {
		t.Fatalf("blocked resume must carry pending_rollback: %v", resu)
	}

	_, file := apiDo(t, http.MethodGet, apiBase(ts)+"/fs/file?path=keep.txt&conversation="+id+"&token=test-token", nil)
	st, fsResp := apiDo(t, http.MethodPut, apiBase(ts)+"/fs/file?token=test-token", map[string]any{
		"path": "keep.txt", "content": "hacked\n", "base_hash": anyString(file["hash"]), "conversation": id,
	})
	if st != http.StatusConflict {
		t.Fatalf("fs write should be blocked, status=%d %v", st, fsResp)
	}
	if mustRead(t, work, "keep.txt") != "keep\n" {
		t.Fatalf("blocked fs write still mutated file: %q", mustRead(t, work, "keep.txt"))
	}

	rollbackApplyHook = nil
	rollbackCompensateHook = nil
	st, rec = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/rollback/recover?token=test-token", map[string]any{})
	if st != http.StatusOK {
		t.Fatalf("recover after hooks cleared: %d %v", st, rec)
	}
	if rec["pending"] == true {
		t.Fatalf("recover should have finished compensation: %v", rec)
	}
	if mustRead(t, work, "a.txt") != "A-new\n" || mustRead(t, work, "b.txt") != "B-new\n" {
		t.Fatalf("compensation should restore pre-rollback files, a=%q b=%q", mustRead(t, work, "a.txt"), mustRead(t, work, "b.txt"))
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("successful compensation must not mark rolled_back_turns: %v", c.RolledBackTurns)
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared after successful recover compensation")
	}
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if got["pending_rollback"] != nil {
		t.Fatalf("pending_rollback still present: %v", got["pending_rollback"])
	}
}

func TestRollbackJournal_ExclusiveLeaseBlocksConcurrentWrites(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, map[string]string{"keep.txt": "keep\n"})

	t.Cleanup(func() { rollbackApplyHook = nil; rollbackCompensateHook = nil })
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rollbackApplyHook = func(path string) error {
		if path == "a.txt" {
			once.Do(func() { close(started) })
			<-release
		}
		return nil
	}

	ts := apiServer(t, work, data)
	type rbResult struct {
		st   int
		resp map[string]any
		err  error
	}
	done := make(chan rbResult, 1)
	go func() {
		st, resp, err := apiDoRawStatus(http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/rollback?token=test-token", map[string]any{"confirm": true})
		done <- rbResult{st, resp, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback did not reach the exclusive-write hook")
	}

	_, file := apiDo(t, http.MethodGet, apiBase(ts)+"/fs/file?path=keep.txt&conversation="+id+"&token=test-token", nil)
	st, fsResp := apiDo(t, http.MethodPut, apiBase(ts)+"/fs/file?token=test-token", map[string]any{
		"path": "keep.txt", "content": "during-rollback\n", "base_hash": anyString(file["hash"]), "conversation": id,
	})
	if st != http.StatusConflict {
		t.Fatalf("fs write during exclusive rollback want 409, got %d %v", st, fsResp)
	}
	if mustRead(t, work, "keep.txt") != "keep\n" {
		t.Fatal("exclusive lease failed to protect keep.txt")
	}

	close(release)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.st != http.StatusOK {
			t.Fatalf("rollback after lease wait: %d %v", got.st, got.resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rollback did not finish")
	}
}

func apiDoRawStatus(method, url string, body any) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, nil
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(sub) == 0 ||
		containsFoldAt(s, sub))
}

func containsFoldAt(s, sub string) bool {
	return bytes.Contains(bytes.ToLower([]byte(s)), bytes.ToLower([]byte(sub)))
}
