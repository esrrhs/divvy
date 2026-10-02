package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/engine"
	"github.com/esrrhs/go_llm_engine/pkg/llm"
)

type traceEvent map[string]any

func readEventStream(t *testing.T, path string) []traceEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open event stream %s: %v", path, err)
	}
	defer f.Close()
	var out []traceEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e traceEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("invalid event JSON %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e traceEvent) str(key string) string {
	if v, ok := e[key].(string); ok {
		return v
	}
	return ""
}

func eventsOfKind(events []traceEvent, kind string) []traceEvent {
	var out []traceEvent
	for _, e := range events {
		if e.str("kind") == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestSessionTrace_EventStreamAndLog runs a full two-leaf session and asserts
// the JSONL event stream carries every event kind needed for post-mortems,
// and that the human-readable log file is populated.
func TestSessionTrace_EventStreamAndLog(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{Handle: twoLeafWorkerHandle(nil)}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_trace"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := readEventStream(t, filepath.Join(data, "events", "test_trace.jsonl"))
	if len(events) == 0 {
		t.Fatal("no events recorded")
	}

	starts := eventsOfKind(events, "session_start")
	if len(starts) != 1 || starts[0].str("mode") != "run" {
		t.Fatalf("expected one run session_start, got %v", starts)
	}
	if starts[0].str("model") == "" || starts[0].str("workdir") != work {
		t.Fatalf("session_start missing config: %v", starts[0])
	}
	ends := eventsOfKind(events, "session_end")
	if len(ends) != 1 || ends[0].str("outcome") != "completed" {
		t.Fatalf("expected completed session_end, got %v", ends)
	}
	if _, ok := ends[0]["duration_ms"]; !ok {
		t.Fatal("session_end missing duration_ms")
	}

	// LLM calls for both foreman (decompose) and workers, with summaries.
	kinds := map[string]bool{}
	for _, e := range eventsOfKind(events, "llm_call") {
		kinds[e.str("call_kind")] = true
		if n, _ := e["n_messages"].(float64); n < 2 {
			t.Fatalf("llm_call missing message summaries: %v", e)
		}
		if _, ok := e["duration_ms"]; !ok {
			t.Fatal("llm_call missing duration_ms")
		}
	}
	if !kinds["decompose"] || !kinds["worker"] {
		t.Fatalf("expected decompose+worker llm calls, got %v", kinds)
	}

	// Tool calls carry the tool name, args summary, and ok status.
	var sawWrite bool
	for _, e := range eventsOfKind(events, "tool_call") {
		if e.str("tool") == "write_file" {
			sawWrite = true
			if ok, _ := e["ok"].(bool); !ok {
				t.Fatalf("write_file recorded as not ok: %v", e)
			}
			if !strings.Contains(e.str("args"), "a.txt") && !strings.Contains(e.str("args"), "b.txt") {
				t.Fatalf("tool_call args missing path: %v", e)
			}
		}
	}
	if !sawWrite {
		t.Fatal("no write_file tool_call recorded")
	}

	// State transitions reach COMPLETED.
	var completed bool
	for _, e := range eventsOfKind(events, "state_change") {
		if e.str("to") == "COMPLETED" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("no state_change into COMPLETED")
	}

	// Verification commands are recorded with their verdict.
	var verified bool
	for _, e := range eventsOfKind(events, "verify") {
		if ok, _ := e["ok"].(bool); ok && strings.HasPrefix(e.str("command"), "test -f") {
			verified = true
		}
	}
	if !verified {
		t.Fatal("no passing verify event for test -f commands")
	}

	// The plain-text log mirrors screen output with timestamps.
	logData, err := os.ReadFile(filepath.Join(data, "logs", "test_trace.log"))
	if err != nil {
		t.Fatalf("run log missing: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "INFO") || !strings.Contains(logText, "execute") {
		t.Fatalf("run log lacks expected content:\n%s", logText)
	}
}

// atomicMarkerPlan decomposes into a single atomic leaf whose DoD requires a
// marker file the worker only creates on its second attempt.
const atomicMarkerPlan = `{
  "is_atomic": true,
  "reason": "one file plus marker",
  "contract": {"outputs": ["a.txt", "marker"]},
  "dod": {"commands": ["test -f marker"]},
  "subtasks": []
}`

func toolResultSeen(req llm.Request, tool string) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool "+tool+" result") {
			return true
		}
	}
	return false
}

// TestSessionTrace_RetryKeepsErrorHistory verifies a failed first attempt is
// recorded as a retry event, persisted in the node's ErrorHistory, survives a
// tree reload, and eventually completes.
func TestSessionTrace_RetryKeepsErrorHistory(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{Content: atomicMarkerPlan}, nil
		}
		user := ""
		if len(req.Messages) > 1 {
			user = req.Messages[1].Content
		}
		// Attempt 2 starts a fresh context but carries the previous
		// verification failure; only then does the worker create the marker.
		retrying := strings.Contains(user, "Previous attempt failed")
		switch {
		case toolResultSeen(req, "run_bash"):
			return jsonAction("finish", map[string]string{"summary": "marker created"}), nil
		case retrying:
			return jsonAction("run_bash", map[string]string{"command": "touch marker"}), nil
		case toolResultSeen(req, "write_file"):
			return jsonAction("finish", map[string]string{"summary": "file written"}), nil
		default:
			return jsonAction("write_file", map[string]string{"path": "a.txt", "content": "alpha"}), nil
		}
	}}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_retry_trace"
	cfg.Goal = "write then marker"
	cfg.Stream = false
	cfg.MaxRetries = 2
	cfg.RetryMinInterval = time.Millisecond
	cfg.RetryMaxInterval = 2 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("run should succeed after retry: %v", err)
	}

	events := readEventStream(t, filepath.Join(data, "events", "test_retry_trace.jsonl"))
	retries := eventsOfKind(events, "retry")
	if len(retries) == 0 {
		t.Fatal("expected at least one retry event")
	}
	if n, _ := retries[0]["attempt"].(float64); n != 1 {
		t.Fatalf("first retry attempt = %v, want 1", retries[0]["attempt"])
	}
	if !strings.Contains(retries[0].str("reason"), "marker") {
		t.Fatalf("retry reason should quote failing verify output: %v", retries[0])
	}

	var sawFailedVerify bool
	for _, e := range eventsOfKind(events, "verify") {
		if ok, _ := e["ok"].(bool); !ok && strings.Contains(e.str("command"), "marker") {
			sawFailedVerify = true
		}
	}
	if !sawFailedVerify {
		t.Fatal("expected a failing verify event for the marker")
	}

	root := o.tree.GetRoot()
	if len(root.ErrorHistory) == 0 {
		t.Fatal("expected root node to keep error history after retry")
	}
	if root.ErrorHistory[0].Error == "" || root.ErrorHistory[0].Time.IsZero() {
		t.Fatalf("error record incomplete: %+v", root.ErrorHistory[0])
	}

	// The history must survive the persisted tree snapshot for later review.
	storage, err := engine.NewStorage(data)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := storage.LoadTree("test_retry_trace")
	if err != nil {
		t.Fatal(err)
	}
	rn, ok := reloaded.GetNode(reloaded.RootID)
	if !ok {
		t.Fatal("reloaded root missing")
	}
	if len(rn.ErrorHistory) == 0 {
		t.Fatal("error history not persisted")
	}
}
