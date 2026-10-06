package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

func apiServer(t *testing.T, work, data string) *httptest.Server {
	t.Helper()
	return apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return srvScript() })
}

func apiServerWithClient(t *testing.T, work, data string,
	factory func(agent.Config) llm.Client) *httptest.Server {
	t.Helper()
	s, err := New(agent.NewRunManager(), data,
		WithToken("test-token"), WithHeartbeat(50*time.Millisecond),
		WithWorkdir(work), WithClientFactory(factory))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func apiDo(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func apiBase(ts *httptest.Server) string { return ts.URL + "/api" }

func createSimpleSession(t *testing.T, ts *httptest.Server, extra map[string]any) (string, map[string]any) {
	t.Helper()
	body := map[string]any{"goal": "write out file"}
	for k, v := range extra {
		body[k] = v
	}
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions?token=test-token", body)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id, _ := resp["session_id"].(string)
	if id == "" {
		t.Fatalf("no session id in %v", resp)
	}
	return id, resp
}

func livePhase(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	status, resp := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("get session status=%d resp=%v", status, resp)
	}
	live, _ := resp["live"].(map[string]any)
	return live
}

func waitLivePhase(t *testing.T, ts *httptest.Server, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if live := livePhase(t, ts, id); live != nil && live["phase"] == want {
			return live
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("live phase never reached %s (last=%v)", want, livePhase(t, ts, id))
	return nil
}

// TR-5.1: create requires a goal; created session appears in the list and
// detail views; after completion the persisted session is still readable.
func TestAPI_CreateListGet(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions?token=test-token",
		map[string]any{"model": "x"})
	if status != http.StatusBadRequest {
		t.Fatalf("missing goal: status=%d resp=%v", status, resp)
	}

	id, created := createSimpleSession(t, ts, nil)
	if phase, _ := created["phase"].(map[string]any); phase["phase"] != string(agent.PhasePlanReview) &&
		phase["phase"] != string(agent.PhasePlanning) {
		t.Fatalf("fresh session phase: %v", created)
	}

	// Wait for the first checkpoint before listing — the tree is persisted
	// when planning completes, not at HTTP create time.
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))

	// List: the new session with its live phase.
	status, listResp := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	items, _ := listResp["sessions"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 session, got %v", items)
	}
	row := items[0].(map[string]any)
	if row["id"] != id || row["goal"] != "write out file" {
		t.Fatalf("list row wrong: %v", row)
	}
	if live, _ := row["live"].(map[string]any); live["phase"] == nil {
		t.Fatalf("list row must carry live phase: %v", row)
	}

	// Detail carries the tree.
	status, detail := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusOK || detail["tree"] == nil {
		t.Fatalf("detail status=%d tree=%v", status, detail["tree"])
	}

	// Approve and run to completion.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("approve status=%d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseDone))

	// Live handle gone but history still readable from storage.
	status, hist := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusOK || hist["tree"] == nil {
		t.Fatalf("history detail status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("workdir file missing: %v", err)
	}
}

// TR-5.2: approve / adjust / abort drive the guider over HTTP.
func TestAPI_PlanActions(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()

	// abort path
	{
		ts := apiServer(t, work, data)
		id, _ := createSimpleSession(t, ts, nil)
		waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
		status, _ := apiDo(t, http.MethodPost,
			apiBase(ts)+"/sessions/"+id+"/abort?token=test-token", map[string]any{})
		if status != http.StatusOK {
			t.Fatalf("abort status=%d", status)
		}
		live := waitLivePhase(t, ts, id, string(agent.PhaseFailed))
		if !strings.Contains(anyString(live["error"]), "aborted") {
			t.Fatalf("abort error: %v", live)
		}
		if _, err := os.Stat(filepath.Join(work, "out.txt")); !os.IsNotExist(err) {
			t.Fatal("aborted plan must not execute")
		}
	}

	// adjust → approve path (fresh workdir to avoid the aborted file)
	work2, data2 := t.TempDir(), t.TempDir()
	ts := apiServer(t, work2, data2)
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work2})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/adjust?token=test-token",
		map[string]any{"comment": "just one tiny file"})
	if status != http.StatusOK {
		t.Fatalf("adjust status=%d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("approve status=%d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseDone))
}

// Actions against an unknown/finished session are 404/409.
func TestAPI_ActionStateProtection(t *testing.T) {
	_, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, t.TempDir(), data)

	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/nope/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("unknown session action: %d, want 404", status)
	}

	work := t.TempDir()
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	// add/pause/answer are invalid while parked at plan review → 409.
	for _, action := range []string{"add", "pause", "answer"} {
		status, _ = apiDo(t, http.MethodPost,
			apiBase(ts)+"/sessions/"+id+"/"+action+"?token=test-token",
			map[string]any{"instruction": "x", "text": "x"})
		if status != http.StatusConflict {
			t.Fatalf("%s at review: %d, want 409", action, status)
		}
	}
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/abort?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseFailed))
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/abort?token=test-token", map[string]any{})
	if status != http.StatusConflict {
		t.Fatalf("action after finish: %d, want 409", status)
	}
}

// Same workdir cannot host two live sessions: 409 from POST /sessions.
func TestAPI_WorkdirConflict(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id0, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id0, string(agent.PhasePlanReview))
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions?token=test-token",
		map[string]any{"goal": "second", "parallel": 2})
	if status != http.StatusConflict {
		t.Fatalf("second start: status=%d resp=%v", status, resp)
	}
}

// Log paging returns bounded line windows.
func TestAPI_LogPaging(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseDone))

	status, page := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/log?token=test-token&limit=1&offset=0", nil)
	if status != http.StatusOK {
		t.Fatalf("log status=%d", status)
	}
	lines, _ := page["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("first page lines=%v", lines)
	}
	if hasMore, _ := page["has_more"].(bool); !hasMore {
		t.Fatalf("a completed run writes more than one log line: %v", page)
	}
	next, _ := page["next_offset"].(float64)
	status, page2 := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/log?token=test-token&limit=1&offset="+itoa(int64(next)), nil)
	if status != http.StatusOK {
		t.Fatalf("log page 2 status=%d", status)
	}
	l2, _ := page2["lines"].([]any)
	if len(l2) != 1 || l2[0] == lines[0] {
		t.Fatalf("page 2 must advance: %v vs %v", l2, lines)
	}
}

// Pause then resume over HTTP continues a saved run.
func TestAPI_PauseAndResume(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	mgr := agent.NewRunManager()
	s, err := New(mgr, data, WithToken("test-token"),
		WithHeartbeat(50*time.Millisecond), WithWorkdir(work),
		WithClientFactory(func(agent.Config) llm.Client { return gatedScript(gate) }))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cfg := srvCfg(work, data, "api_resume")
	// A second live start against the same workdir must collide.
	cfg.Goal = "pause resume test"
	h, err := mgr.Start(cfg, gatedScript(gate), agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitRunPhase(t, h, agent.PhasePlanReview)

	id := h.SessionID()
	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseRunning))
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/pause?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("pause: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhasePaused))
	close(gate) // interrupted worker unblocks

	// Resume: planning restarts (no leaves completed) and parks at review.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/resume?token=test-token",
		map[string]any{"model": cfg.Model})
	if status != http.StatusOK {
		t.Fatalf("resume: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseDone))
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("resumed run did not finish the work: %v", err)
	}
}

// Manual leaf approval end-to-end through the HTTP API: reject the first
// diff with a comment, approve the re-done work.
func TestAPI_LeafApprovalDiff(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServerWithClient(t, work, data,
		func(agent.Config) llm.Client { return approvalHTTPScript() })
	id, _ := createSimpleSession(t, ts, map[string]any{
		"approval_mode": "manual", // backend must auto-enable isolate
	})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})

	live := waitLivePhase(t, ts, id, string(agent.PhaseLeafApproval))
	leafID, _ := live["leaf_approving"].(string)
	if leafID == "" {
		t.Fatalf("leaf approval phase without leaf id: %v", live)
	}

	// First diff shows "first".
	status, approval := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("get approval: %d %v", status, approval)
	}
	changes, _ := approval["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %v", changes)
	}
	first := changes[0].(map[string]any)
	if first["new_content"] != "first" || first["path"] != "out.txt" {
		t.Fatalf("first diff wrong: %v", first)
	}

	// Rejecting without a comment is a 400.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token",
		map[string]any{"decision": "reject"})
	if status != http.StatusBadRequest {
		t.Fatalf("reject without comment: %d, want 400", status)
	}

	// Reject with the comment; the leaf redoes and parks again.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token",
		map[string]any{"decision": "reject", "comment": "must say second"})
	if status != http.StatusOK {
		t.Fatalf("reject: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseLeafApproval))
	status, approval2 := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("get approval 2: %d", status)
	}
	for _, c := range approval2["changes"].([]any) {
		cm := c.(map[string]any)
		if cm["path"] == "out.txt" && cm["new_content"] != "second" {
			t.Fatalf("redone diff must show 'second': %v", cm)
		}
	}

	// Bad decision value → 400.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token",
		map[string]any{"decision": "maybe"})
	if status != http.StatusBadRequest {
		t.Fatalf("bad decision: %d", status)
	}

	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/approvals/"+leafID+"?token=test-token",
		map[string]any{"decision": "approve"})
	if status != http.StatusOK {
		t.Fatalf("approve leaf: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseDone))
	if got, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(got) != "second" {
		t.Fatalf("workdir must hold approved re-done content: %q %v", got, err)
	}
}

// approvalHTTPScript writes "first" until the rejection comment is fed
// back, then "second".
func approvalHTTPScript() llm.Client {
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, "You are a software architect for a weak coding model.") {
				return &llm.Response{Content: srvAtomicPlan}, nil
			}
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
					return &llm.Response{Content: `{"action":"finish","args":{"summary":"done"}}`}, nil
				}
			}
			content := "first"
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			if strings.Contains(user, "REVIEWED AND REJECTED") {
				content = "second"
			}
			return &llm.Response{Content: `{"action":"write_file","args":{"path":"out.txt","content":"` + content + `"}}`}, nil
		},
	}
}

func gatedScript(gate <-chan struct{}) llm.Client {
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, "You are a software architect for a weak coding model.") {
				return &llm.Response{Content: srvAtomicPlan}, nil
			}
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
					return &llm.Response{Content: `{"action":"finish","args":{"summary":"done"}}`}, nil
				}
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &llm.Response{Content: `{"action":"write_file","args":{"path":"out.txt","content":"hi\n"}}`}, nil
		},
	}
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}
