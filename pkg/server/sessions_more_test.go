package server

import (
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

// TestAPI_ActionUnknownSession404 walks every action endpoint against a
// session id that is not running: each handler must answer 404 instead of
// leaking a panic or a 500.
func TestAPI_ActionUnknownSession404(t *testing.T) {
	ts := apiServer(t, t.TempDir(), t.TempDir())
	endpoints := []string{
		"/sessions/nope/plan/approve",
		"/sessions/nope/plan/adjust",
		"/sessions/nope/abort",
		"/sessions/nope/pause",
		"/sessions/nope/redo",
		"/sessions/nope/add",
		"/sessions/nope/answer",
		"/sessions/nope/approvals/leaf_1",
	}
	for _, ep := range endpoints {
		method := http.MethodPost
		if strings.HasPrefix(ep, "/sessions/nope/approvals") {
			method = http.MethodGet
		}
		status, _ := apiDo(t, method, apiBase(ts)+ep+"?token=test-token",
			map[string]any{"comment": "x", "node_id": "leaf_1", "text": "x", "decision": "approve"})
		if status != http.StatusNotFound {
			t.Errorf("%s on unknown session: %d, want 404", ep, status)
		}
	}
}

// TestAPI_ActionBadBodies covers the JSON decode and decision guards of the
// action handlers against a live session.
func TestAPI_ActionBadBodies(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))

	// Invalid JSON bodies are 400s.
	status, _ := apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/adjust?token=test-token", "not json")
	if status != http.StatusBadRequest {
		t.Fatalf("adjust bad body: %d", status)
	}
	status, _ = apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/redo?token=test-token", "{")
	if status != http.StatusBadRequest {
		t.Fatalf("redo bad body: %d", status)
	}
	status, _ = apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/add?token=test-token", "{")
	if status != http.StatusBadRequest {
		t.Fatalf("add bad body: %d", status)
	}
	status, _ = apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/answer?token=test-token", "{")
	if status != http.StatusBadRequest {
		t.Fatalf("answer bad body: %d", status)
	}
	// Create with a broken body is a 400 too.
	status, _ = apiDoRaw(t, http.MethodPost, apiBase(ts)+"/sessions?token=test-token", "{")
	if status != http.StatusBadRequest {
		t.Fatalf("create bad body: %d", status)
	}
	// Resume of an unknown session is 404; a broken body is 400.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/nope/resume?token=test-token", map[string]any{"goal": "x"})
	if status != http.StatusNotFound {
		t.Fatalf("resume unknown: %d", status)
	}
	status, _ = apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/nope/resume?token=test-token", "{")
	if status != http.StatusBadRequest {
		t.Fatalf("resume bad body: %d", status)
	}
}

// apiDoRaw posts a raw string body and returns the status code.
func apiDoRaw(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, map[string]any{}
}

// TestAPI_LiveSessionEmptyLogPage covers the empty-page fallback for a
// session whose handle is still registered but whose log file is gone
// (e.g. wiped between runs while the terminal handle lingers).
func TestAPI_LiveSessionEmptyLogPage(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseDone))

	// Remove the log behind the lingered handle; the handle still exists,
	// so the endpoint must answer an empty page instead of 404.
	if err := os.Remove(filepath.Join(data, "logs", id+".log")); err != nil {
		t.Fatal(err)
	}
	status, page := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/log?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("live empty log: %d", status)
	}
	if lines, _ := page["lines"].([]any); len(lines) != 0 {
		t.Fatalf("expected an empty page, got %v", page)
	}
}

// TestAPI_LogPagingValidation covers the offset/limit guards and the query
// fallback for malformed values.
func TestAPI_LogPagingValidation(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseDone))

	// Unknown session: 404.
	if status, _ := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/absent/log?token=test-token", nil); status != http.StatusNotFound {
		t.Fatalf("absent log: %d", status)
	}
	// Bad offset/limit: 400.
	for _, q := range []string{"offset=-1", "limit=0", "limit=-5"} {
		if status, _ := apiDo(t, http.MethodGet,
			apiBase(ts)+"/sessions/"+id+"/log?token=test-token&"+q, nil); status != http.StatusBadRequest {
			t.Fatalf("query %q: %d, want 400", q, status)
		}
	}
	// Malformed numbers fall back to defaults; the huge limit is clamped.
	status, page := apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/log?token=test-token&offset=abc&limit=99999", nil)
	if status != http.StatusOK {
		t.Fatalf("fallback query: %d", status)
	}
	if _, ok := page["lines"]; !ok {
		t.Fatalf("fallback page missing lines: %v", page)
	}
}

// TestAPI_SessionListingErrors covers the 500s when the data directory is
// unusable and the 404 for a session that was never created.
func TestAPI_SessionListingErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("block"), 0644); err != nil {
		t.Fatal(err)
	}
	// dataDir is a file → storage creation fails.
	ts := apiServer(t, t.TempDir(), blocker)
	status, _ := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("list with broken dataDir: %d, want 500", status)
	}
	status, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/some-id?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("get with broken dataDir: %d, want 500", status)
	}

	// Healthy server, unknown session → 404.
	ts2 := apiServer(t, t.TempDir(), t.TempDir())
	status, _ = apiDo(t, http.MethodGet, apiBase(ts2)+"/sessions/absent?token=test-token", nil)
	if status != http.StatusNotFound {
		t.Fatalf("absent session: %d, want 404", status)
	}
}

// TestServer_ConfigFromParamsFull exercises every optional request field and
// the manual-approval isolation coupling.
func TestServer_ConfigFromParamsFull(t *testing.T) {
	s, err := New(agent.NewRunManager(), t.TempDir(), WithToken("t"), WithWorkdir("ws-default"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.configFromParams(sessionParams{
		Goal: "g", WorkDir: "ws", Model: "m",
		BaseURL: "http://api.example.com/v1/", APIKey: "k",
		Parallel: 3, Isolate: false, GitCommit: true, Web: true, Browser: true,
		SearchURL: "http://s.example/?q={query}", ApprovalMode: "manual",
		BudgetTokens: 100, MaxCost: 2, MaxRetries: 1, MaxDepth: 2,
		MaxSubtasks: 3, MaxStall: 4, MaxTokens: 5, Temperature: 0.7,
	})
	if cfg.Goal != "g" || cfg.WorkDir != "ws" || cfg.Model != "m" {
		t.Fatalf("basic fields lost: %+v", cfg)
	}
	if cfg.BaseURL != "http://api.example.com/v1" {
		t.Fatalf("trailing slash must be trimmed: %q", cfg.BaseURL)
	}
	if cfg.APIKey != "k" || cfg.Parallel != 3 || !cfg.GitCommit || !cfg.WebEnabled || !cfg.BrowserEnabled {
		t.Fatalf("params not applied: %+v", cfg)
	}
	if cfg.SearchURL != "http://s.example/?q={query}" || cfg.LeafApproval != "manual" {
		t.Fatalf("optional fields lost: %+v", cfg)
	}
	if !cfg.Isolate {
		t.Fatal("manual approval must force isolate mode")
	}
	if cfg.BudgetTokens != 100 || cfg.MaxCost != 2 || cfg.MaxRetries != 1 ||
		cfg.MaxDepth != 2 || cfg.MaxSubtasks != 3 || cfg.MaxStall != 4 ||
		cfg.MaxTokens != 5 || cfg.Temperature != 0.7 {
		t.Fatalf("budget fields lost: %+v", cfg)
	}

	// Empty params keep the server defaults (workdir from WithWorkdir).
	def := s.configFromParams(sessionParams{})
	if def.WorkDir != "ws-default" || def.Goal != "" || def.Parallel <= 0 {
		t.Fatalf("defaults wrong: %+v", def)
	}
}

// TestServer_NewLLMClient pins the client wiring that the default server
// path uses (tests normally inject scripted clients).
func TestServer_NewLLMClient(t *testing.T) {
	c := newLLMClient(agent.Config{
		APIKey: "k", BaseURL: "http://x/v1", RequestTimeout: 3 * time.Second,
		ExtraJSON: `{"a":1}`, RetryMaxInterval: 2 * time.Second,
	})
	oc, ok := c.(*llm.OpenAIClient)
	if !ok {
		t.Fatalf("newLLMClient must build an OpenAIClient, got %T", c)
	}
	if oc.ExtraJSON != `{"a":1}` || oc.MaxBackoff != 2*time.Second {
		t.Fatalf("client wiring wrong: extra=%q backoff=%v", oc.ExtraJSON, oc.MaxBackoff)
	}
}

// TestAPI_AddInstructionWhileRunning covers the happy path of the add
// endpoint while the run is executing (the worker is held on a gate).
func TestAPI_AddInstructionWhileRunning(t *testing.T) {
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

	cfg := agent.DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.Goal = "add while paused"
	cfg.Stream = false
	h, err := mgr.Start(cfg, gatedScript(gate), agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitRunPhase(t, h, agent.PhasePlanReview)
	id := h.SessionID()
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseRunning))

	// The worker is parked on the gate, so the run is live but idle.
	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/add?token=test-token",
		map[string]any{"instruction": "also add a README"})
	if status != http.StatusOK {
		t.Fatalf("add while running: %d, want 200", status)
	}
	close(gate)
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
}
