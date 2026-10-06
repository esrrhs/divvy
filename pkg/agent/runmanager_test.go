package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

const mgrAtomicPlan = `{
  "is_atomic": true,
  "reason": "one file",
  "contract": {"outputs": ["out.txt"]},
  "dod": {"commands": ["test -f out.txt"]},
  "subtasks": []
}`

const mgrTwoLeafPlan = `{
  "is_atomic": false,
  "reason": "two files",
  "subtasks": [
    {"id": "a", "title": "A", "description": "write a", "type": "LEAF",
     "contract": {"outputs": ["a.txt"]}, "dod": {"commands": ["test -f a.txt"]}},
    {"id": "b", "title": "B", "description": "write b", "type": "LEAF",
     "contract": {"outputs": ["b.txt"], "dependencies": ["a"]}, "dod": {"commands": ["test -f b.txt"]}}
  ]
}`

func mgrCfg(work, data, session string) Config {
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = session
	cfg.Goal = "web driven run"
	cfg.Stream = false
	cfg.DecomposeTries = 2
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond
	return cfg
}

// fileWritingScript maps the running leaf's task id to its output file; each
// leaf turn is write_file then finish. planOverride selects the architect
// plan; gate, when non-nil, blocks the first worker turn until released or
// the context dies (used by the pause test).
func fileWritingScript(plan string, gate <-chan struct{}) llm.Client {
	return &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: plan}, nil
			}
			user := ""
			if len(req.Messages) > 1 {
				user = req.Messages[1].Content
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			}
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			name := "out.txt"
			switch {
			// Match the full line: a /add leaf titled "also..." would
			// otherwise satisfy the "Task ID: a" prefix.
			case strings.Contains(user, "Task ID: a\n"):
				name = "a.txt"
			case strings.Contains(user, "Task ID: b\n"):
				name = "b.txt"
			default:
				// User-added leaves (/add): give them an output the generic
				// DoD fallback can verify regardless of their empty contract.
				if strings.Contains(user, "extra") {
					name = "extra.txt"
				}
			}
			return jsonAction("write_file", map[string]string{"path": name, "content": name + "\n"}), nil
		},
	}
}

func waitPhase(t *testing.T, h *RunHandle, want RunPhase, timeout time.Duration) RunPhase {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p := h.Phase(); p == want {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase never reached %s (last snapshot: %+v)", want, h.Snapshot())
	return ""
}

// TR-3.1: the whole new → plan approve → complete path is driven through
// channels, with no terminal involved.
func TestRunManager_StartApproveComplete(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := NewRunManager()

	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_start"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if got := h.Snapshot(); got.Phase != PhasePlanReview {
		t.Fatalf("snapshot: %+v", got)
	}
	if err := h.ApprovePlan(); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.Phase() != PhaseDone {
		t.Fatalf("phase = %s, want done", h.Phase())
	}
	if got, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(got) != "out.txt\n" {
		t.Fatalf("workdir content wrong: %q %v", got, err)
	}
	if _, err := os.Stat(h.Orchestrator().EventPath()); err != nil {
		t.Fatalf("event stream missing: %v", err)
	}
}

// TR-3.2: a second live run for the same workdir conflicts; a different
// workdir runs in parallel.
func TestRunManager_WorkdirConflict(t *testing.T) {
	work1, work2, data := t.TempDir(), t.TempDir(), t.TempDir()
	mgr := NewRunManager()

	h1, err := mgr.Start(mgrCfg(work1, filepath.Join(data, "1"), "s1"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	h2, err := mgr.Start(mgrCfg(work2, filepath.Join(data, "2"), "s2"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatalf("different workdir must run in parallel: %v", err)
	}

	_, err = mgr.Start(mgrCfg(work1, filepath.Join(data, "3"), "s3"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if !errors.Is(err, ErrWorkdirBusy) {
		t.Fatalf("want ErrWorkdirBusy, got %v", err)
	}

	waitPhase(t, h1, PhasePlanReview, 5*time.Second)
	waitPhase(t, h2, PhasePlanReview, 5*time.Second)
	if err := h1.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h2.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h1.Wait(); err != nil {
		t.Fatalf("h1: %v", err)
	}
	if err := h2.Wait(); err != nil {
		t.Fatalf("h2: %v", err)
	}

	// Once finished the workdir lock frees: starting again is allowed.
	h3, err := mgr.Start(mgrCfg(work1, filepath.Join(data, "4"), "s4"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatalf("workdir should be reusable after finish: %v", err)
	}
	waitPhase(t, h3, PhasePlanReview, 5*time.Second)
	_ = h3.Abort()
	_ = h3.Wait()
}

// TR-3.3 adjust: feedback replans, the review round opens again, approval
// then completes the run.
func TestRunManager_AdjustThenApprove(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_adjust"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.AdjustPlan("keep it to one file"); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	// An adjusted plan parks at review again.
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.Phase() != PhaseDone {
		t.Fatalf("phase = %s", h.Phase())
	}
}

// TR-3.3 abort at review: plan is saved unexecuted and the run fails with
// the guider's abort error; the workspace stays untouched.
func TestRunManager_AbortAtReview(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_abort"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := h.Wait(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("want abort error, got %v", err)
	}
	if h.Phase() != PhaseFailed {
		t.Fatalf("phase = %s, want failed", h.Phase())
	}
	if _, err := os.Stat(filepath.Join(work, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("aborted plan must not execute, out.txt exists: %v", err)
	}
}

// TR-3.3 pause: while a leaf is running, pause checkpoints and ends the run
// resumably (ErrPaused); the workdir lock is released.
func TestRunManager_PauseWhileRunning(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	gate := make(chan struct{})
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_pause"),
		fileWritingScript(mgrAtomicPlan, gate), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseRunning, 5*time.Second)
	if err := h.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := h.Wait(); err != ErrPaused {
		t.Fatalf("want ErrPaused, got %v", err)
	}
	if h.Phase() != PhasePaused {
		t.Fatalf("phase = %s, want paused", h.Phase())
	}
	// Busy map released even though the handle stays registered.
	if ids := mgr.ActiveSessions(); len(ids) != 1 || ids[0] != h.SessionID() {
		t.Fatalf("handle registration: %v", ids)
	}
	close(gate) // release the interrupted worker; nothing should restart
}

// TR-3.3 add: an instruction queued during execution becomes a new leaf that
// runs after the planned leaves.
func TestRunManager_AddInstruction(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_add"),
		fileWritingScript(mgrTwoLeafPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseRunning, 5*time.Second)
	if err := h.AddInstruction("also write an extra marker file"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, f := range []string{"a.txt", "b.txt", "extra.txt"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Fatalf("expected %s after /add: %v", f, err)
		}
	}
}

// TR-3.3 redo: a /redo queued during execution resets the finished leaf so it
// runs one more time, then the run completes.
func TestRunManager_RedoQueued(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var writeCalls int64
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: mgrAtomicPlan}, nil
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			}
			atomic.AddInt64(&writeCalls, 1)
			return jsonAction("write_file", map[string]string{"path": "out.txt", "content": "x\n"}), nil
		},
	}
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_redo"), client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseRunning, 5*time.Second)
	if err := h.Redo(h.Orchestrator().tree.RootID); err != nil {
		t.Fatalf("redo: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt64(&writeCalls); got != 2 {
		t.Fatalf("leaf should run twice after redo, wrote %d times", got)
	}
	leaf, _ := h.Orchestrator().tree.CloneNode(h.Orchestrator().tree.RootID)
	if leaf == nil || leaf.State != models.TaskStateCompleted {
		t.Fatalf("leaf state: %+v", leaf)
	}
}

// TR-3.3 answer: a worker ask parks the run; the posted answer resumes it and
// the exchange is recorded in the event stream.
func TestRunManager_AnswerQuestion(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: mgrAtomicPlan}, nil
			}
			// The ask answer is appended as a later user message in the same
			// worker turn — scanning only messages[1] would re-ask forever.
			answered := false
			for _, m := range req.Messages {
				if strings.Contains(m.Content, "User answer:") {
					answered = true
				}
			}
			switch {
			case reqHasToolResult(req):
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			case answered:
				return jsonAction("write_file", map[string]string{"path": "out.txt", "content": "port 9090\n"}), nil
			default:
				return jsonAction("ask", map[string]string{"question": "which port should I use?"}), nil
			}
		},
	}
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_answer"), client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseAsk, 5*time.Second)
	if q := h.Snapshot().PendingAsk; !strings.Contains(q, "which port") {
		t.Fatalf("pending ask: %q", q)
	}
	if err := h.Answer("9090"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	ev, err := os.ReadFile(h.Orchestrator().EventPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ev), "which port") || !strings.Contains(string(ev), "9090") {
		t.Fatalf("ask/answer not recorded in events:\n%s", ev)
	}
}

// State guards: actions outside their phase are rejected with ErrWrongPhase,
// and every action is rejected after the run ends (ErrRunFinished).
func TestRunManager_PhaseGuards(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_guards"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)

	for _, act := range []struct {
		name string
		fn   func() error
	}{
		{"add", func() error { return h.AddInstruction("x") }},
		{"redo", func() error { return h.Redo("root") }},
		{"pause", func() error { return h.Pause() }},
		{"answer", func() error { return h.Answer("x") }},
		{"decide", func() error { return h.DecideLeaf("root", true, "") }},
	} {
		if err := act.fn(); !errors.Is(err, ErrWrongPhase) {
			t.Errorf("%s at review: want ErrWrongPhase, got %v", act.name, err)
		}
	}
	// Empty payloads are argument errors, not phase errors.
	if err := h.AdjustPlan("  "); err == nil || errors.Is(err, ErrWrongPhase) {
		t.Errorf("empty adjust: want argument error, got %v", err)
	}

	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := h.ApprovePlan(); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("post-finish approve: want ErrRunFinished, got %v", err)
	}
	if err := h.Abort(); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("post-finish abort: want ErrRunFinished, got %v", err)
	}
}
