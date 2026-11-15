package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

// FR-9: a finished session can be deleted with its tree, log and event
// stream; it then disappears from the list and detail views.
func TestAPI_DeleteFinishedSession(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)

	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	waitLivePhase(t, ts, id, string(agent.PhaseDone))

	for _, name := range []string{id + ".json", filepath.Join("events", id+".jsonl")} {
		if _, err := os.Stat(filepath.Join(data, name)); err != nil {
			t.Fatalf("precondition %s missing: %v", name, err)
		}
	}

	status, resp := apiDo(t, http.MethodDelete,
		apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, resp)
	}
	removed, _ := resp["removed"].([]any)
	if len(removed) < 1 {
		t.Fatalf("delete must report removed files: %v", resp)
	}

	// Tree, event stream and log are gone.
	for _, name := range []string{id + ".json", filepath.Join("events", id+".jsonl"), filepath.Join("logs", id+".log")} {
		if _, err := os.Stat(filepath.Join(data, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be deleted (err=%v)", name, err)
		}
	}

	// Detail and list no longer know it; the lingering live handle is
	// dropped too, so the list carries no phase for the id.
	status, _ = apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusNotFound {
		t.Fatalf("detail after delete: %d, want 404", status)
	}
	status, listResp := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	for _, it := range listResp["sessions"].([]any) {
		if it.(map[string]any)["id"] == id {
			t.Fatal("deleted session still listed")
		}
	}

	// Deleting again is a 404.
	status, _ = apiDo(t, http.MethodDelete,
		apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusNotFound {
		t.Fatalf("repeat delete: %d, want 404", status)
	}
}

// A run still in progress must not be deleted out from under itself.
func TestAPI_DeleteRunningSessionConflict(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, work, data)
	id, _ := createSimpleSession(t, ts, nil)
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))

	status, _ := apiDo(t, http.MethodDelete,
		apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusConflict {
		t.Fatalf("deleting an active session: %d, want 409", status)
	}
	// The tree must survive the refused delete.
	if _, err := os.Stat(filepath.Join(data, id+".json")); err != nil {
		t.Fatalf("active session tree must survive: %v", err)
	}
}

// A paused session is terminal in the phase machine (its worker has exited
// and the tree is fully checkpointed), so it may be deleted; afterwards
// resume cannot revive it.
func TestAPI_DeletePausedSession(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	mgr := agent.NewRunManager()
	defer mgr.Shutdown(2 * time.Second)
	s, err := New(mgr, data, WithToken("test-token"), WithHeartbeat(50*time.Millisecond),
		WithWorkdir(work), WithClientFactory(func(agent.Config) llm.Client { return gatedScript(gate) }))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cfg := srvCfg(work, data, "api_delete_paused")
	cfg.Goal = "pause then delete test"
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
	close(gate) // unblock the interrupted worker so serve() winds down

	status, resp := apiDo(t, http.MethodDelete,
		apiBase(ts)+"/sessions/"+id+"?token=test-token", nil)
	if status != http.StatusOK {
		t.Fatalf("delete paused: %d %v", status, resp)
	}
	if _, err := os.Stat(filepath.Join(data, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("paused session tree should be gone: %v", err)
	}
	// Resume can no longer load it.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/resume?token=test-token", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("resume after delete: %d, want 404", status)
	}
}

// Unknown and malformed ids get 404/400 instead of touching the disk.
func TestAPI_DeleteSessionErrors(t *testing.T) {
	_, data := t.TempDir(), t.TempDir()
	ts := apiServer(t, t.TempDir(), data)

	status, _ := apiDo(t, http.MethodDelete,
		apiBase(ts)+"/sessions/does_not_exist?token=test-token", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", status)
	}
	for _, bad := range []string{"..%2fsomething", "a%2Fb", "%2e%2e", "x%20y"} {
		status, _ = apiDo(t, http.MethodDelete,
			apiBase(ts)+"/sessions/"+bad+"?token=test-token", nil)
		if status != http.StatusBadRequest {
			t.Fatalf("bad id %q: %d, want 400", bad, status)
		}
	}
}
