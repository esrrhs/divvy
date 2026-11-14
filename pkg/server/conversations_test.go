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
	"github.com/esrrhs/divvy/pkg/llm"
)

const testSecretKey = "sk-test-secret-key-do-not-persist"

func waitConvPhase(t *testing.T, fn func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = fn()
		if last == want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("conversation phase never reached %s (last=%q)", want, last)
}

func TestConversation_FollowUpLoopAndKeyRedaction(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir":       work,
		"model":         "scripted",
		"base_url":      "http://127.0.0.1:9/v1",
		"api_key":       testSecretKey,
		"isolate":       true,
		"approval_mode": "auto",
		"message":       "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("missing conversation id: %v", resp)
	}
	if resp["has_api_key"] != true {
		t.Fatalf("has_api_key should be true: %v", resp)
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), testSecretKey) {
		t.Fatalf("API key leaked in create response: %s", raw)
	}
	if note, _ := resp["context_note"].(string); !strings.Contains(note, "不是无限完整记忆") {
		t.Fatalf("missing context note: %v", resp)
	}

	sid, _ := resp["current_session_id"].(string)
	if sid == "" {
		t.Fatalf("first message should start a session: %v", resp)
	}
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})

	waitConv := func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		phase, _ := got["run_phase"].(string)
		// After the handle is still around, phase is "done".
		if phase != "" {
			return phase
		}
		// Fall back to the last finished outcome once the handle is gone.
		msgs, _ := got["messages"].([]any)
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i].(map[string]any)
			if m["kind"] == kindRunFinished {
				if m["outcome"] == "completed" {
					return string(agent.PhaseDone)
				}
				return anyString(m["outcome"])
			}
		}
		return phase
	}
	waitConvPhase(t, waitConv, string(agent.PhaseDone))

	gotPath := filepath.Join(work, "out.txt")
	body, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("expected workspace file after first turn: %v", err)
	}
	if string(body) != "hi\n" {
		t.Fatalf("out.txt=%q, want hi\\n", body)
	}

	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	raw, _ = json.Marshal(got)
	if strings.Contains(string(raw), testSecretKey) {
		t.Fatalf("API key leaked in GET conversation: %s", raw)
	}
	disk, err := os.ReadFile(conversationPath(data, id))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), testSecretKey) {
		t.Fatalf("API key persisted on disk: %s", disk)
	}
	msgs, _ := got["messages"].([]any)
	var kinds []string
	for _, item := range msgs {
		m := item.(map[string]any)
		kinds = append(kinds, anyString(m["kind"]))
		if m["role"] == "assistant" {
			t.Fatalf("logs must not be stored as assistant replies: %v", m)
		}
	}
	if !containsString(kinds, kindUser) || !containsString(kinds, kindRunStarted) || !containsString(kinds, kindRunFinished) {
		t.Fatalf("message kinds=%v, want user/run_started/run_finished", kinds)
	}

	// Follow-up in the same conversation: no model/key fields required.
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write out file again",
	})
	if status != http.StatusAccepted {
		t.Fatalf("follow-up status=%d resp=%v", status, resp)
	}
	sid2, _ := resp["current_session_id"].(string)
	if sid2 == "" || sid2 == sid {
		t.Fatalf("follow-up should start a new session, got %q (prev %q)", sid2, sid)
	}
	waitLivePhase(t, ts, sid2, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid2+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, waitConv, string(agent.PhaseDone))

	// Phase can flip to done before watchConversationRun appends the
	// second work-log line; wait for the transcript to catch up.
	var log []any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, got = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		log, _ = got["work_log"].([]any)
		if len(log) >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if len(log) < 2 {
		t.Fatalf("work log should accumulate turns, got %v", log)
	}
	// The second session goal must carry the compressed earlier turn.
	st, detail := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+sid2+"?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("get follow-up session: %d %v", st, detail)
	}
	treeJSON, _ := json.Marshal(detail["tree"])
	if !strings.Contains(string(treeJSON), "Conversation memory") &&
		!strings.Contains(string(treeJSON), "write out file") {
		t.Fatalf("follow-up session should include earlier-turn context, tree=%s", treeJSON)
	}
}

func TestConversation_RejectsMessageWhileRunningOrPaused(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir":  work,
		"model":    "scripted",
		"base_url": "http://127.0.0.1:9/v1",
		"api_key":  testSecretKey,
		"isolate":  true,
		"message":  "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "another change",
		"mode": "queue",
	})
	if status != http.StatusAccepted {
		t.Fatalf("running post should queue, status=%d resp=%v", status, resp)
	}
	if resp["queue"] == nil {
		t.Fatalf("expected queued follow-up, got %v", resp)
	}
	status, resp = apiDo(t, http.MethodDelete, apiBase(ts)+"/conversations/"+id+"/queue?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel queue status=%d resp=%v", status, resp)
	}
	if resp["queue"] != nil {
		t.Fatalf("queue should be cleared, got %v", resp["queue"])
	}

	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, sid, string(agent.PhaseRunning))

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/pause?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("pause status=%d resp=%v", status, resp)
	}
	// Pause is async: wait until the conversation records paused, or the
	// live phase becomes paused.
	deadline := time.Now().Add(8 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if got["run_phase"] == string(agent.PhasePaused) {
			paused = true
			break
		}
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "paused" {
				paused = true
				break
			}
		}
		if paused {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !paused {
		t.Fatal("session did not reach paused")
	}

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "another change",
	})
	if status != http.StatusConflict {
		t.Fatalf("paused post: status=%d resp=%v", status, resp)
	}

	// Resume through the conversation endpoint (settings/key reused).
	// A pause mid-leaf (no completed leaves) resumes into plan review again;
	// that is existing guided-run behavior, not a skip-to-execute path.
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/resume?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("resume status=%d resp=%v", status, resp)
	}
	sid = anyString(resp["current_session_id"])
	if sid == "" {
		sid = anyString(resp["run_session_id"])
	}
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	close(gate)
	waitConv := func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if p := anyString(got["run_phase"]); p != "" {
			return p
		}
		msgs, _ := got["messages"].([]any)
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i].(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}
	waitConvPhase(t, waitConv, string(agent.PhaseDone))
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("resumed run should still write the file: %v", err)
	}
}

func TestConversation_BrowseAndRejectMissingDir(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	sub := filepath.Join(work, "proj")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "not-a-dir"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	ts := apiServer(t, work, data)

	// The API reports the resolved directory (macOS temp dirs live under
	// /private/var), so ask about the resolved form.
	resolved, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	status, resp := apiDo(t, http.MethodGet, apiBase(ts)+"/browse?token=test-token&path="+resolved, nil)
	if status != http.StatusOK {
		t.Fatalf("browse status=%d resp=%v", status, resp)
	}
	if resp["path"] != resolved {
		t.Fatalf("browse path=%v", resp["path"])
	}
	names := map[string]string{}
	for _, item := range resp["entries"].([]any) {
		e := item.(map[string]any)
		names[anyString(e["name"])] = anyString(e["type"])
	}
	if names["proj"] != "dir" {
		t.Fatalf("expected proj dir in browse: %v", names)
	}

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": filepath.Join(work, "missing-project"),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("missing dir: status=%d resp=%v", status, resp)
	}
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": filepath.Join(work, "not-a-dir"),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("file as workdir: status=%d resp=%v", status, resp)
	}

	// Auth still required on the new routes.
	status, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauth list: status=%d", status)
	}
	status, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/browse?path="+work, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauth browse: status=%d", status)
	}
}

func TestFollowUpGoalAndWorkLogHelpers(t *testing.T) {
	if g := followUpGoal("do thing", "", "", ""); g != "do thing" {
		t.Fatalf("no log: %q", g)
	}
	g := followUpGoal("next", "1. User: first | Outcome: completed", "", "")
	if !strings.Contains(g, "next") || !strings.Contains(g, "Conversation memory") || !strings.Contains(g, "first") {
		t.Fatalf("unexpected goal: %q", g)
	}
	c := &Conversation{}
	for i := 0; i < 6; i++ {
		c.appendMessage("user", kindUser, "request-"+string(rune('A'+i))+" unique-"+string(rune('0'+i)), "", "", nil)
		c.appendMessage("system", kindRunFinished, "result-"+string(rune('A'+i)), "", "completed", nil)
		c.Messages[len(c.Messages)-1].Changes = []turnChangeSummary{{Path: "f" + string(rune('0'+i)) + ".txt", Status: "added"}}
	}
	mem := conversationMemory(c)
	if !strings.Contains(mem, "request-F") || !strings.Contains(mem, "Recent turns") {
		t.Fatalf("recent detail missing: %q", mem)
	}
	if !strings.Contains(mem, "request-A") || !strings.Contains(mem, "added:f0.txt") {
		t.Fatalf("older compact missing: %q", mem)
	}
	entry := compactTurn("write out file", "completed", "done", nil)
	if !strings.Contains(entry, "User: write out file") || !strings.Contains(entry, "completed") {
		t.Fatalf("compact: %q", entry)
	}
}

func TestConversation_QueueStartsAfterCurrentRun(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write out file again",
		"mode": "queue",
	})
	if status != http.StatusAccepted {
		t.Fatalf("queue status=%d resp=%v", status, resp)
	}

	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	close(gate)

	// First run finishes, queued turn should auto-start a new session.
	deadline := time.Now().Add(8 * time.Second)
	var sid2 string
	for time.Now().Before(deadline) {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		cur := anyString(got["current_session_id"])
		if cur != "" && cur != sid {
			if live := livePhase(t, ts, cur); live != nil && live["phase"] == string(agent.PhasePlanReview) {
				sid2 = cur
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if sid2 == "" {
		t.Fatal("queued follow-up did not auto-start")
	}
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid2+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		// Wait until the second run is finished and no queue remains.
		if got["queue"] != nil {
			return "queued"
		}
		msgs, _ := got["messages"].([]any)
		finished := 0
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				finished++
			}
		}
		if finished >= 2 {
			return "done-2"
		}
		return anyString(got["run_phase"])
	}, "done-2")
}

func TestConversation_TakeoverAbortsCurrent(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write out file now",
		"mode": "takeover",
	})
	if status != http.StatusAccepted {
		t.Fatalf("takeover status=%d resp=%v", status, resp)
	}
	sid2 := anyString(resp["current_session_id"])
	if sid2 == "" || sid2 == sid {
		t.Fatalf("takeover should start a new session, got %q (old %q) resp=%v", sid2, sid, resp)
	}
	waitLivePhase(t, ts, sid2, string(agent.PhasePlanReview))
	close(gate)
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid2+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		var aborted, completed bool
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "aborted" {
				aborted = true
			}
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				completed = true
			}
		}
		if aborted && completed {
			return "ok"
		}
		return anyString(got["run_phase"])
	}, "ok")
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("takeover run should write the file: %v", err)
	}
}

func TestConversation_FileContextAndTurnChanges(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "hello.go"), []byte("package src\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "blob.bin"), []byte{0, 1, 2, 3}, 0644); err != nil {
		t.Fatal(err)
	}
	ts := apiServer(t, work, data)

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])

	status, resp = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/files?token=test-token&q=hello", nil)
	if status != http.StatusOK {
		t.Fatalf("files status=%d resp=%v", status, resp)
	}
	entries, _ := resp["entries"].([]any)
	found := false
	for _, item := range entries {
		e := item.(map[string]any)
		if anyString(e["path"]) == "src/hello.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search missed src/hello.go: %v", resp)
	}

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text":  "write out file using the referenced helper",
		"paths": []string{"src/hello.go", "blob.bin", "../etc/passwd", "/etc/passwd"},
	})
	if status != http.StatusAccepted {
		t.Fatalf("message status=%d resp=%v", status, resp)
	}
	// API key / escaped path must not leak into the conversation payload.
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "root:") || strings.Contains(string(raw), "/etc/shadow") {
		t.Fatalf("unexpected secret-like content: %s", raw)
	}
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	st, detail := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+sid+"?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("get session: %d %v", st, detail)
	}
	treeJSON, _ := json.Marshal(detail["tree"])
	goal := string(treeJSON)
	if !strings.Contains(goal, "package src") || !strings.Contains(goal, "src/hello.go") {
		t.Fatalf("goal missing referenced file content: %s", treeJSON)
	}
	if !strings.Contains(goal, "Automatic project context") && !strings.Contains(goal, "Project map") {
		t.Fatalf("goal missing automatic project context: %s", treeJSON)
	}
	if strings.Contains(goal, "root:x") || strings.Contains(goal, "daemon:") {
		t.Fatalf("escaped path content leaked into goal")
	}
	if !strings.Contains(goal, "binary") {
		t.Fatalf("binary file should be noted as omitted: %s", treeJSON)
	}

	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if anyString(got["run_phase"]) == string(agent.PhaseDone) {
			return string(agent.PhaseDone)
		}
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))

	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	msgs, _ := got["messages"].([]any)
	var sawChanges bool
	for _, item := range msgs {
		m := item.(map[string]any)
		if m["kind"] != kindRunFinished {
			continue
		}
		changes, _ := m["changes"].([]any)
		for _, ch := range changes {
			cm := ch.(map[string]any)
			if anyString(cm["path"]) == "out.txt" && anyString(cm["status"]) == "added" {
				sawChanges = true
			}
		}
	}
	if !sawChanges {
		t.Fatalf("run_finished should list out.txt as added, messages=%v", got["messages"])
	}
	st, ch := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("changes status=%d resp=%v", st, ch)
	}
	list, _ := ch["changes"].([]any)
	var foundDiff bool
	for _, item := range list {
		cm := item.(map[string]any)
		if anyString(cm["path"]) == "out.txt" && strings.Contains(anyString(cm["new_content"]), "hi") {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Fatalf("stored turn diff missing out.txt content: %v", ch)
	}
}

func TestCollectContextRejectsEscape(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "ok.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	block, refs, err := collectContext(work, []string{"ok.txt", "../ok.txt", "/etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "hello") {
		t.Fatalf("missing ok.txt content: %s", block)
	}
	var escaped int
	for _, r := range refs {
		if r.Excluded != "" && r.Path != "ok.txt" {
			escaped++
		}
	}
	if escaped < 1 {
		t.Fatalf("expected escaped paths to be excluded, refs=%+v", refs)
	}
	if strings.Contains(block, "root:") {
		t.Fatal("escaped file content was included")
	}
}

func TestConversation_SteerDuringPlanReviewAndRunning(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	// Default mid-run send injects into the current turn (replan), not 409 and not queue.
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "please also cover the helper",
	})
	if status != http.StatusAccepted {
		t.Fatalf("steer at review status=%d resp=%v", status, resp)
	}
	if resp["queue"] != nil {
		t.Fatalf("steer must not occupy the next-turn queue: %v", resp)
	}
	if !steerNoticeed(resp, "steered") {
		t.Fatalf("expected steered notice, messages=%v", resp["messages"])
	}
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, sid, string(agent.PhaseRunning))

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "also write an extra marker after this leaf",
		"mode": "steer",
	})
	if status != http.StatusAccepted {
		t.Fatalf("steer while running status=%d resp=%v", status, resp)
	}
	if resp["queue"] != nil {
		t.Fatalf("running steer must not queue a new turn: %v", resp)
	}
	close(gate)
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if anyString(got["run_phase"]) == string(agent.PhaseDone) {
			return string(agent.PhaseDone)
		}
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("original work should remain: %v", err)
	}
	st, detail := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+sid+"?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("get session: %d %v", st, detail)
	}
	treeJSON, _ := json.Marshal(detail["tree"])
	if !strings.Contains(string(treeJSON), "extra marker") && !strings.Contains(string(treeJSON), "also write") {
		t.Fatalf("added instruction should appear in the tree, tree=%s", treeJSON)
	}
}

func TestCollectProjectContextBoundedAndSafe(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "hello.go"), []byte("package hello\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "node_modules", "secret.js"), []byte("should-not-appear"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "blob.bin"), []byte{0, 1, 2, 3}, 0644); err != nil {
		t.Fatal(err)
	}
	got := collectProjectContext(work, "please use the hello helper")
	if !strings.Contains(got, "Project map") || !strings.Contains(got, "src/hello.go") {
		t.Fatalf("map missing hello.go: %s", got)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("keyword hit missing: %s", got)
	}
	if strings.Contains(got, "should-not-appear") || strings.Contains(got, "node_modules") {
		t.Fatalf("skipped dir leaked: %s", got)
	}
	if len(got) > autoTotalMaxBytes+1024 {
		t.Fatalf("automatic context too large: %d", len(got))
	}
}

func steerNoticeed(resp map[string]any, outcome string) bool {
	msgs, _ := resp["messages"].([]any)
	for _, item := range msgs {
		m, _ := item.(map[string]any)
		if m["kind"] == kindNotice && anyString(m["outcome"]) == outcome {
			return true
		}
		if anyString(m["outcome"]) == outcome {
			return true
		}
	}
	return false
}

func TestConversation_HunkAcceptRejectWritesWorkdir(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write out file",
	})
	if status != http.StatusAccepted {
		t.Fatalf("message status=%d resp=%v", status, resp)
	}
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("run should have written out.txt: %v", err)
	}

	var st int
	var ch map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, ch = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
		if st == http.StatusOK && ch["editable"] == true {
			if list, _ := ch["changes"].([]any); len(list) > 0 {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if st != http.StatusOK {
		t.Fatalf("changes status=%d resp=%v", st, ch)
	}
	if ch["editable"] != true {
		t.Fatalf("latest finished turn should be editable: %v", ch)
	}

	// Reject the added out.txt hunks: the file should leave the workdir.
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "out.txt", "all": "reject",
	})
	if st != http.StatusOK {
		t.Fatalf("reject status=%d resp=%v", st, resp)
	}
	if _, err := os.Stat(filepath.Join(work, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected add should remove out.txt, err=%v", err)
	}
	// Re-accept from the same snapshot: file comes back.
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "out.txt", "all": "accept",
	})
	if st != http.StatusOK {
		t.Fatalf("accept status=%d resp=%v", st, resp)
	}
	body, err := os.ReadFile(filepath.Join(work, "out.txt"))
	if err != nil || string(body) != "hi\n" {
		t.Fatalf("accepted content=%q err=%v", body, err)
	}

	// A follow-up turn makes the first session historical / read-only.
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "write out file again",
	})
	if st != http.StatusAccepted {
		t.Fatalf("follow-up status=%d resp=%v", st, resp)
	}
	sid2 := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid2, string(agent.PhasePlanReview))
	st, ch = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
	if st != http.StatusOK || ch["editable"] == true {
		t.Fatalf("historical turn must be read-only: status=%d resp=%v", st, ch)
	}
	st, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "out.txt", "all": "reject",
	})
	if st != http.StatusConflict {
		t.Fatalf("historical apply should 409, got %d %v", st, resp)
	}
}

func TestConversation_HunkApplyBlockedWhileRunning(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "out.txt", "all": "reject",
	})
	if st != http.StatusConflict {
		t.Fatalf("apply during plan review should 409, got %d %v", st, resp)
	}
	close(gate)
}

func TestConversation_SteerEscapesCurrentLeaf(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return gatedScript(gate) })
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, sid, string(agent.PhaseRunning))
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "also finish sooner", "mode": "steer",
	})
	if st != http.StatusAccepted {
		t.Fatalf("steer status=%d resp=%v", st, resp)
	}
	close(gate)
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))
	st, detail := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+sid+"?token=test-token", nil)
	if st != http.StatusOK {
		t.Fatalf("session %d %v", st, detail)
	}
	treeJSON, _ := json.Marshal(detail["tree"])
	if !strings.Contains(string(treeJSON), "finish sooner") && !strings.Contains(string(treeJSON), "also finish") {
		t.Fatalf("steered leaf missing from tree: %s", treeJSON)
	}
}

func TestConversation_LocalhostNeedsNoKeyAndRemoteDoes(t *testing.T) {
	work, work2, data := t.TempDir(), t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("localhost without key should start, status=%d resp=%v", status, resp)
	}
	if resp["key_required"] == true {
		t.Fatalf("localhost must not require a key: %v", resp)
	}
	if resp["has_api_key"] == true {
		t.Fatalf("no key was provided: %v", resp)
	}
	sid := anyString(resp["current_session_id"])
	if sid == "" {
		t.Fatalf("turn should have started: %v", resp)
	}
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/abort?token=test-token", map[string]any{})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+anyString(resp["id"])+"?token=test-token", nil)
		phase := anyString(got["run_phase"])
		if phase == "" || phase == string(agent.PhaseFailed) || phase == string(agent.PhaseDone) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work2, "model": "gpt-4o-mini", "base_url": "https://api.openai.com/v1",
		"isolate": true, "message": "write out file",
	})
	if status != http.StatusPreconditionRequired {
		t.Fatalf("remote without key should be 428, got %d %v", status, resp)
	}
	errText := anyString(resp["error"]) + fmtAny(resp)
	if !strings.Contains(errText, "API key") && !strings.Contains(errText, "密钥") {
		t.Fatalf("expected key required error, got %d %v", status, resp)
	}
	if strings.Contains(fmtAny(resp), testSecretKey) {
		t.Fatalf("test secret leaked: %s", fmtAny(resp))
	}
	if conv, ok := resp["conversation"].(map[string]any); ok {
		id := anyString(conv["id"])
		if id != "" {
			disk, err := os.ReadFile(conversationPath(data, id))
			if err == nil && (strings.Contains(string(disk), testSecretKey) || strings.Contains(string(disk), "sk-")) {
				t.Fatalf("key persisted: %s", disk)
			}
		}
	}
}

func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestConversation_HunkApplyRejectsPathEscape(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))

	st, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "../outside.txt", "all": "accept",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("escaped path should 400, got %d %v", st, got)
	}
}

func TestConversation_HunkApplyConcurrentAndQueuePersist(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client {
		return fileWritingConvScript()
	})
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"isolate": true, "message": "write two files",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))

	// Queue a follow-up while the plan is still in review, then cancel it
	// after we persist-check the file. First prove the queue is on disk.
	st, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "queued follow-up", "mode": "queue",
	})
	if st != http.StatusAccepted {
		t.Fatalf("queue status=%d %v", st, resp)
	}
	disk, err := os.ReadFile(conversationPath(data, id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "queued follow-up") {
		t.Fatalf("queued text should persist on disk: %s", disk)
	}
	if strings.Contains(string(disk), testSecretKey) {
		t.Fatalf("key must not be in the conversation file")
	}
	apiDo(t, http.MethodDelete, apiBase(ts)+"/conversations/"+id+"/queue?token=test-token", nil)

	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))

	// Wait until both files are in the stored diff.
	deadline := time.Now().Add(5 * time.Second)
	var ch map[string]any
	for time.Now().Before(deadline) {
		_, ch = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
		list, _ := ch["changes"].([]any)
		if ch["editable"] == true && len(list) >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if ch["editable"] != true {
		t.Fatalf("expected editable two-file diff, got %v", ch)
	}

	type result struct {
		st   int
		body map[string]any
	}
	out := make(chan result, 2)
	go func() {
		st, body := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
			"path": "a.txt", "all": "reject",
		})
		out <- result{st, body}
	}()
	go func() {
		st, body := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
			"path": "b.txt", "all": "accept",
		})
		out <- result{st, body}
	}()
	for i := 0; i < 2; i++ {
		r := <-out
		if r.st != http.StatusOK {
			t.Fatalf("concurrent hunk apply failed: %d %v", r.st, r.body)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt should be rejected/removed, err=%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "b.txt")); err != nil || string(b) != "b.txt\n" {
		t.Fatalf("b.txt should remain accepted: %q %v", b, err)
	}

	// Same-file concurrent applies must not 5xx or tear the file.
	done := make(chan result, 2)
	go func() {
		st, body := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
			"path": "b.txt", "all": "reject",
		})
		done <- result{st, body}
	}()
	go func() {
		st, body := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
			"path": "b.txt", "all": "accept",
		})
		done <- result{st, body}
	}()
	var statuses []int
	for i := 0; i < 2; i++ {
		r := <-done
		statuses = append(statuses, r.st)
		if r.st >= 500 {
			t.Fatalf("same-file concurrent apply must not 5xx: %d %v", r.st, r.body)
		}
	}
	// One may win with 200, the other may 409 if the workdir no longer matches.
	okish := 0
	for _, s := range statuses {
		if s == http.StatusOK || s == http.StatusConflict {
			okish++
		}
	}
	if okish != 2 {
		t.Fatalf("unexpected same-file statuses %v", statuses)
	}
	if raw, err := os.ReadFile(filepath.Join(work, "b.txt")); err == nil && string(raw) != "b.txt\n" && string(raw) != "" {
		t.Fatalf("torn or unexpected b.txt content: %q", raw)
	}
}

func fileWritingConvScript() llm.Client {
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys, user := "", ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			if strings.Contains(sys, "You are a software architect for a weak coding model.") {
				return &llm.Response{Content: `{
  "is_atomic": false,
  "reason": "two files",
  "subtasks": [
    {"id": "a", "title": "A", "description": "write a", "type": "LEAF",
     "contract": {"outputs": ["a.txt"]}, "dod": {"commands": ["test -f a.txt"]}},
    {"id": "b", "title": "B", "description": "write b", "type": "LEAF",
     "contract": {"outputs": ["b.txt"], "dependencies": ["a"]}, "dod": {"commands": ["test -f b.txt"]}}
  ]
}`}, nil
			}
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
					return &llm.Response{Content: `{"action":"finish","args":{"summary":"done"}}`}, nil
				}
			}
			name := "out.txt"
			switch {
			case strings.Contains(user, "Task ID: a\n"):
				name = "a.txt"
			case strings.Contains(user, "Task ID: b\n"):
				name = "b.txt"
			}
			return &llm.Response{Content: `{"action":"write_file","args":{"path":"` + name + `","content":"` + name + `\n"}}`}, nil
		},
	}
}

func TestConversation_HunkApplyRefusesExternalEdits(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"isolate": true, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/plan/approve?token=test-token", map[string]any{})
	waitConvPhase(t, func() string {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		msgs, _ := got["messages"].([]any)
		for _, item := range msgs {
			m := item.(map[string]any)
			if m["kind"] == kindRunFinished && m["outcome"] == "completed" {
				return string(agent.PhaseDone)
			}
		}
		return anyString(got["run_phase"])
	}, string(agent.PhaseDone))

	deadline := time.Now().Add(5 * time.Second)
	var ch map[string]any
	for time.Now().Before(deadline) {
		_, ch = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
		if ch["editable"] == true {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if ch["editable"] != true {
		t.Fatalf("expected editable changes, got %v", ch)
	}
	if err := os.WriteFile(filepath.Join(work, "out.txt"), []byte("my own edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, got := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", map[string]any{
		"path": "out.txt", "all": "reject",
	})
	if st != http.StatusConflict {
		t.Fatalf("hunk apply must not overwrite an out-of-band edit, got %d %v", st, got)
	}
	body, err := os.ReadFile(filepath.Join(work, "out.txt"))
	if err != nil || string(body) != "my own edit\n" {
		t.Fatalf("user edit was changed: %q %v", body, err)
	}
}

func TestConversation_SessionGoalRedactsAPIKey(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	msg := "please keep " + testSecretKey + " out of memory and write out file"
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "isolate": true, "message": msg,
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	if strings.Contains(fmtAny(resp), testSecretKey) {
		t.Fatalf("key leaked in create response: %s", fmtAny(resp))
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
	// The session tree (model context) is already persisted at Start.
	treePath := filepath.Join(data, sid+".json")
	if _, err := os.Stat(treePath); err != nil {
		// Some layouts nest the tree; search the datadir.
		found := ""
		_ = filepath.Walk(data, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.Contains(p, sid) {
				found = p
			}
			return nil
		})
		if found == "" {
			t.Fatalf("session tree for %s not found under %s", sid, data)
		}
		treePath = found
	}
	raw, err := os.ReadFile(treePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testSecretKey) {
		t.Fatalf("API key persisted in session/model context %s", treePath)
	}
	if ev, err := os.ReadFile(filepath.Join(data, "events", sid+".jsonl")); err == nil && strings.Contains(string(ev), testSecretKey) {
		t.Fatalf("API key persisted in event stream")
	}
	if lg, err := os.ReadFile(filepath.Join(data, "logs", sid+".log")); err == nil && strings.Contains(string(lg), testSecretKey) {
		t.Fatalf("API key persisted in session log")
	}
	disk, err := os.ReadFile(conversationPath(data, id))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), testSecretKey) {
		t.Fatalf("API key persisted in conversation file")
	}
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/abort?token=test-token", map[string]any{})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		phase := anyString(got["run_phase"])
		if phase == "" || phase == string(agent.PhaseFailed) || phase == string(agent.PhaseDone) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
}
