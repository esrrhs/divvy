package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

const srvAtomicPlan = `{
  "is_atomic": true,
  "reason": "one file",
  "contract": {"outputs": ["out.txt"]},
  "dod": {"commands": ["test -f out.txt"]},
  "subtasks": []
}`

// srvScript is an external-package copy of the in-package approval script:
// architect turn returns the plan, worker writes out.txt then finishes.
func srvScript() llm.Client {
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, "You are a software architect for a weak coding model.") {
				return &llm.Response{Content: srvAtomicPlan}, nil
			}
			// Tool results are fed back as a user message ("Tool <name>
			// result ..."), not a tool-role message.
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
					return &llm.Response{Content: `{"action":"finish","args":{"summary":"done"}}`}, nil
				}
			}
			return &llm.Response{Content: `{"action":"write_file","args":{"path":"out.txt","content":"hi\n"}}`}, nil
		},
	}
}

func srvCfg(work, data, session string) agent.Config {
	cfg := agent.DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = session
	cfg.Goal = "sse test"
	cfg.Stream = false
	cfg.DecomposeTries = 2
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond
	return cfg
}

func startSSEServer(t *testing.T, mgr *agent.RunManager, data string) *httptest.Server {
	t.Helper()
	s, err := New(mgr, data, WithToken("test-token"), WithHeartbeat(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// sseFrame is one dispatched SSE message; comment carries heartbeat lines.
type sseFrame struct {
	id      string
	data    string
	comment string
}

func readFrame(r *bufio.Reader) (sseFrame, error) {
	var f sseFrame
	dispatched := false
	for !dispatched {
		line, err := r.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.data != "" || f.comment != "" {
				dispatched = true
			}
		case strings.HasPrefix(line, ":"):
			f.comment = line
		case strings.HasPrefix(line, "id:"):
			f.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			f.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return f, nil
}

func nextFrame(t *testing.T, r *bufio.Reader, timeout time.Duration) sseFrame {
	t.Helper()
	ch := make(chan sseFrame, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := readFrame(r)
		if err != nil {
			errCh <- err
			return
		}
		ch <- f
	}()
	select {
	case f := <-ch:
		return f
	case err := <-errCh:
		t.Fatalf("read frame: %v", err)
	case <-time.After(timeout):
		t.Fatalf("timeout after %s waiting for frame", timeout)
	}
	return sseFrame{}
}

func frameKind(t *testing.T, f sseFrame) (kind string, m map[string]any) {
	t.Helper()
	if f.data == "" {
		return "", nil
	}
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		t.Fatalf("frame data not JSON (%q): %v", f.data, err)
	}
	k, _ := m["kind"].(string)
	return k, m
}

// TR-4.2: connect at plan review → replay + tree snapshot + heartbeat;
// approve → live events; reconnect → replay restores state.
func TestSSE_ReplayLiveHeartbeatAndReconnect(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := agent.NewRunManager()
	h, err := mgr.Start(srvCfg(work, data, "srv_sse"), srvScript(), agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitRunPhase(t, h, agent.PhasePlanReview)

	ts := startSSEServer(t, mgr, data)

	open := func(lastEventID string) (*http.Response, *bufio.Reader) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/srv_sse/events?token=test-token", nil)
		if err != nil {
			t.Fatal(err)
		}
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sse status=%d", resp.StatusCode)
		}
		return resp, bufio.NewReader(resp.Body)
	}

	// First connection: replay must contain the pending plan review, a tree
	// snapshot follows, and a heartbeat arrives while parked.
	resp1, r1 := open("")
	sawReview, sawSnapshot, sawHeartbeat := false, false, false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(sawReview && sawSnapshot && sawHeartbeat) {
		f := nextFrame(t, r1, time.Second)
		if f.comment != "" {
			sawHeartbeat = true
			continue
		}
		kind, _ := frameKind(t, f)
		switch kind {
		case "plan_review":
			sawReview = true
		case "tree_snapshot":
			sawSnapshot = true
		}
	}
	if !sawReview || !sawSnapshot || !sawHeartbeat {
		t.Fatalf("initial stream incomplete: review=%v snapshot=%v heartbeat=%v",
			sawReview, sawSnapshot, sawHeartbeat)
	}

	// Approve and watch the live run transition through state changes and
	// fresh snapshots.
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	sawRunning, sawLiveSnapshot := false, false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sawRunning {
		f := nextFrame(t, r1, time.Second)
		kind, m := frameKind(t, f)
		if kind == "state_change" && (m["to"] == "RUNNING" || m["to"] == "COMPLETED") {
			sawRunning = true
		}
		if kind == "tree_snapshot" {
			sawLiveSnapshot = true
		}
	}
	if !sawRunning {
		t.Fatal("live state_change to RUNNING/COMPLETED never arrived")
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.Phase() != agent.PhaseDone {
		t.Fatalf("phase=%s", h.Phase())
	}
	// Snapshots are coalesced (100ms) and heartbeats interleave, so wait by
	// deadline for the post-approval snapshot instead of counting frames.
	if !sawLiveSnapshot {
		deadline2 := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline2) {
			f := nextFrame(t, r1, time.Second)
			if kind, _ := frameKind(t, f); kind == "tree_snapshot" {
				sawLiveSnapshot = true
				break
			}
		}
	}
	resp1.Body.Close()
	if !sawLiveSnapshot {
		t.Fatal("no live tree_snapshot after approval")
	}

	// Reconnect: the persisted tail restores the final state.
	resp2, r2 := open("")
	defer resp2.Body.Close()
	gotStateChange, gotSnapshot, maxSeq := false, false, int64(0)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !gotSnapshot {
		f := nextFrame(t, r2, time.Second)
		if f.comment != "" {
			continue
		}
		kind, m := frameKind(t, f)
		if kind == "state_change" {
			gotStateChange = true
		}
		if kind == "tree_snapshot" {
			gotSnapshot = true
		}
		if f.id != "" {
			if v, ok := m["seq"].(float64); ok && int64(v) > maxSeq {
				maxSeq = int64(v)
			}
		}
	}
	if !gotStateChange || !gotSnapshot {
		t.Fatalf("reconnect replay lost state: state_change=%v snapshot=%v", gotStateChange, gotSnapshot)
	}

	// Last-Event-ID resume: nothing at or below the last seen seq is replayed.
	resp3, r3 := open(itoa(maxSeq))
	defer resp3.Body.Close()
	for {
		f := nextFrame(t, r3, time.Second)
		if f.comment != "" {
			break // reached the heartbeat phase: replay window is empty
		}
		if f.id != "" {
			// Only the synthetic subscribe snapshot (no id) may follow a
			// current Last-Event-ID; any replayed id must be newer.
			if f.id <= itoa(maxSeq) {
				t.Fatalf("replayed event %s not newer than Last-Event-ID %d", f.id, maxSeq)
			}
			break
		}
	}
}

// SSE itself is behind the token and unknown sessions 404.
func TestSSE_AuthAndMissing(t *testing.T) {
	mgr := agent.NewRunManager()
	ts := startSSEServer(t, mgr, t.TempDir())

	resp, err := http.Get(ts.URL + "/api/sessions/nope/events")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sse without token: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/sessions/nope/events?token=test-token")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TR-4.3: a UI-supplied API key stays in memory and never reaches the
// persisted event stream, logs, or any other session file on disk.
func TestSecretsNeverTouchDisk(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	sentinel := "sk-sentinel-MUST-NOT-LEAK-9f8e7d6c"
	cfg := srvCfg(work, data, "srv_secret")
	cfg.APIKey = sentinel
	mgr := agent.NewRunManager()
	h, err := mgr.Start(cfg, srvScript(), agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitRunPhase(t, h, agent.PhasePlanReview)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}

	err = filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), sentinel) {
			t.Fatalf("api key leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The replay burst delivered over SSE must be clean too: the live handle
	// exposes the key as a redact secret, so even accidental inclusion would
	// be masked before crossing the socket.
	ts := startSSEServer(t, mgr, data)
	resp, err := http.Get(ts.URL + "/api/sessions/srv_secret/events?token=test-token")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	frames := 0
	for time.Now().Before(deadline) && frames < 50 {
		f := nextFrame(t, br, time.Second)
		if strings.Contains(f.data, sentinel) {
			t.Fatalf("api key leaked through SSE: %s", f.data)
		}
		frames++
	}
	_ = resp.Body.Close()
}

func waitRunPhase(t *testing.T, h *agent.RunHandle, want agent.RunPhase) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.Phase() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase never reached %s: %+v", want, h.Snapshot())
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
