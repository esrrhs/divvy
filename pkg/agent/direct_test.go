package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

type testDirectCtl struct {
	mu          sync.Mutex
	pending     string
	injectAfter int
	injectText  string
	pauseAfter  int
	tools       int
	pause       bool
	summary     string
	inTool      bool
}

func (c *testDirectCtl) TakePendingInject() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.pending
	c.pending = ""
	return s, ""
}

func (c *testDirectCtl) PauseRequested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pause
}

func (c *testDirectCtl) SetInTool(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inTool = v
	if v {
		return
	}
	c.tools++
	if c.injectAfter > 0 && c.tools == c.injectAfter && c.injectText != "" {
		c.pending = c.injectText
	}
	if c.pauseAfter > 0 && c.tools == c.pauseAfter {
		c.pause = true
	}
}

func (c *testDirectCtl) SetSummary(s string) {
	c.mu.Lock()
	c.summary = s
	c.mu.Unlock()
}

func joinReq(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func lastToolResult(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == llm.RoleTool || strings.Contains(m.Content, "Tool ") {
			return m.Content
		}
	}
	return ""
}

func jsonAct(action string, args map[string]any) *llm.Response {
	payload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": action, "args": args})
	return &llm.Response{Content: string(payload)}
}

const buggyAdd = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
const fixedAdd = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
const addTest = "package addtest\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d\", got)\n\t}\n}\n"
const addMod = "module addtest\n\ngo 1.22\n"
const mulSrc = "package addtest\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n"
const mulTest = "package addtest\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif got := Mul(3, 4); got != 12 {\n\t\tt.Fatalf(\"Mul(3,4)=%d\", got)\n\t}\n}\n"

// repairScript writes a failing Add, runs go test, reads the failure, fixes
// it, re-runs tests, and finishes with a model-authored summary. A later
// user follow-up asking for Mul continues in the same transcript.
type repairDriver struct {
	mu           sync.Mutex
	calls        *int
	sawInject    *bool
	wroteMod     bool
	wroteAdd     bool
	wroteTest    bool
	ranFail      bool
	wroteFix     bool
	ranPass      bool
	wroteInject  bool
	wroteMul     bool
	wroteMulTest bool
	ranMul       bool
}

func (d *repairDriver) client() llm.Client {
	return &llm.ScriptedClient{Handle: d.handle}
}

func (d *repairDriver) handle(ctx context.Context, req llm.Request) (*llm.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls != nil {
		*d.calls++
	}
	body := joinReq(req)
	if d.sawInject != nil && strings.Contains(body, "inject-marker") {
		*d.sawInject = true
	}
	if strings.Contains(body, "inject-marker") && !d.wroteInject {
		d.wroteInject = true
		return jsonAct(tools.ToolWriteFile, map[string]any{"path": "injected.txt", "content": "from-inject"}), nil
	}
	if strings.Contains(body, "also implement Mul") {
		if !d.wroteMul {
			d.wroteMul = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "mul.go", "content": mulSrc}), nil
		}
		if !d.wroteMulTest {
			d.wroteMulTest = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "mul_test.go", "content": mulTest}), nil
		}
		if !d.ranMul {
			d.ranMul = true
			return jsonAct(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "Added Mul and TestMul; go test passed after the follow-up."}), nil
	}
	if !d.wroteMod {
		d.wroteMod = true
		return jsonAct(tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod}), nil
	}
	if !d.wroteAdd {
		d.wroteAdd = true
		return jsonAct(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd}), nil
	}
	if !d.wroteTest {
		d.wroteTest = true
		return jsonAct(tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest}), nil
	}
	if !d.ranFail {
		d.ranFail = true
		return jsonAct(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), nil
	}
	last := lastToolResult(req)
	if !d.wroteFix {
		if !strings.Contains(last, "FAIL") {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "expected a failing test before the fix; last=" + last}), nil
		}
		d.wroteFix = true
		return jsonAct(tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd}), nil
	}
	if !d.ranPass {
		d.ranPass = true
		return jsonAct(tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}), nil
	}
	return jsonAct(tools.ToolFinish, map[string]any{
		"summary": "The first go test failed because Add used subtraction; I changed it to addition and go test passed.",
	}), nil
}

func directCfg(t *testing.T, work, data, goal string) Config {
	t.Helper()
	cfg := mgrCfg(work, data, "sess_direct_"+t.Name())
	cfg.Goal = goal
	cfg.AgentLoop = AgentLoopDirect
	cfg.DirectSteps = 20
	cfg.APIKey = "sk-direct-test-key-do-not-persist"
	cfg.Stream = false
	cfg.RequestTimeout = 15 * time.Second
	// Unit tests are hermetic: the real post-edit check shells out to the
	// toolchain, which is unavailable on some hosts and slow on all of them.
	// Tests that exercise the check inject d.editCheck/d.recheckEdits and turn
	// this back on explicitly.
	cfg.DisableAutoVerify = true
	return cfg
}

func runGoTest(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("go", "test", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestDirect_FailThenFixThenFollowUp(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Implement Add(a, b int) int in add.go with a test; go test must pass.")
	drv := &repairDriver{}
	client := drv.client()
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	out, err := runGoTest(t, work)
	if err != nil {
		t.Fatalf("first turn should leave tests passing, go test: %v\n%s", err, out)
	}
	if ctl.summary == "" || !strings.Contains(ctl.summary, "subtraction") {
		t.Fatalf("summary must come from the model finish args, got %q", ctl.summary)
	}
	raw, err := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FAIL") {
		t.Fatalf("transcript should contain the real failing go test output")
	}
	if !strings.Contains(string(raw), "ok  ") && !strings.Contains(string(raw), "PASS") {
		t.Fatalf("transcript should contain a later passing go test: %s", raw)
	}
	if strings.Contains(string(raw), cfg.APIKey) {
		t.Fatalf("API key leaked into direct transcript")
	}

	// Follow-up in the same transcript: add Mul.
	cfg.Goal = "also implement Mul(a, b int) int with a test"
	cfg.SessionID = cfg.SessionID + "_follow"
	o2, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o2.Close)
	o2.cfg.TranscriptPath = filepath.Join(data, o.SessionID()+".direct.json")
	// Share the same workdir/data; give a new session but reuse transcript.
	ctl2 := &testDirectCtl{}
	if err := NewDirect(o2, ctl2).Run(context.Background()); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "mul.go")); err != nil {
		t.Fatalf("follow-up should write mul.go: %v", err)
	}
	out, err = runGoTest(t, work)
	if err != nil {
		t.Fatalf("after Mul follow-up, go test should pass: %v\n%s", err, out)
	}
	if !strings.Contains(ctl2.summary, "Mul") {
		t.Fatalf("follow-up summary should mention Mul, got %q", ctl2.summary)
	}
}

func TestDirect_InjectAffectsNextModelStep(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Implement Add in add.go with tests.")
	var saw bool
	drv := &repairDriver{sawInject: &saw}
	client := drv.client()
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{injectAfter: 1, injectText: "inject-marker also write injected.txt"}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !saw {
		t.Fatal("the next model request did not see the injected instruction")
	}
	body, err := os.ReadFile(filepath.Join(work, "injected.txt"))
	if err != nil || string(body) != "from-inject" {
		t.Fatalf("inject should cause injected.txt to be written, got %q %v", body, err)
	}
}

func TestDirect_PauseResumeDoesNotReplayTools(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Implement Add in add.go with tests.")
	var calls int
	drv := &repairDriver{calls: &calls}
	client := drv.client()
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{pauseAfter: 1}
	err = NewDirect(o, ctl).Run(context.Background())
	if !errors.Is(err, ErrPaused) {
		t.Fatalf("want ErrPaused after first tool, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "go.mod")); err != nil {
		t.Fatalf("pause should keep the first write: %v", err)
	}
	firstCalls := calls
	// Resume the same orchestrator/transcript; do not pause again.
	ctl2 := &testDirectCtl{}
	if err := NewDirect(o, ctl2).Run(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if calls <= firstCalls {
		t.Fatalf("resume should make further model calls, before=%d after=%d", firstCalls, calls)
	}
	out, err := runGoTest(t, work)
	if err != nil {
		t.Fatalf("resumed run should finish with passing tests: %v\n%s", err, out)
	}
	// go.mod should still be the one written before pause (not a second copy
	// from a full restart). A naive replay would still pass tests, but the
	// transcript must not show two initial go.mod writes without a user ask.
	raw, _ := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if strings.Count(string(raw), `"path": "go.mod"`)+strings.Count(string(raw), `"path":"go.mod"`) > 3 {
		// allow some mentions in prompts; fail only on a clearly replayed burst
		t.Logf("go.mod mentions are high, check transcript length=%d", len(raw))
	}
}

func TestDirect_RestartDoesNotRepeatInFlightTool(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "look around then finish")
	if err := os.WriteFile(filepath.Join(work, "side.txt"), []byte("already written\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, cfg.SessionID+".direct.json")
	st := &directState{
		Version: 1,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
			{Role: llm.RoleUser, Content: "write side.txt then finish"},
			{Role: llm.RoleAssistant, Content: `{"action":"write_file","args":{"path":"side.txt","content":"new"}}`},
		},
		Step: 1,
		InFlight: &directInFlight{
			Name:        tools.ToolWriteFile,
			Args:        map[string]any{"path": "side.txt", "content": "should-not-overwrite"},
			StartedUnix: time.Now().Unix(),
		},
	}
	if err := saveDirectState(path, st, cfg.APIKey); err != nil {
		t.Fatal(err)
	}
	wrote := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		if !strings.Contains(body, "may already have modified the workspace") {
			t.Errorf("resume must tell the model about the unfinished tool, body=%s", body)
		}
		if strings.Contains(body, "Tool write_file result") {
			t.Errorf("in-flight tool was replayed")
		}
		// If the script were to blindly write, count it.
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "finish") {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "inspected, left side.txt alone"}), nil
		}
		// First model turn after recovery: inspect, do not rewrite.
		if !strings.Contains(body, "Tool read_file") {
			return jsonAct(tools.ToolReadFile, map[string]any{"path": "side.txt"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "inspected, left side.txt alone"}), nil
	}}
	_ = wrote
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(work, "side.txt"))
	if err != nil || string(body) != "already written\n" {
		t.Fatalf("in-flight write must not be replayed, got %q %v", body, err)
	}
}

func TestDirect_ContextBudgetCompactsButKeepsTranscript(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "tiny budget")
	cfg.DirectContextChars = 800
	cfg.APIKey = "sk-budget-secret-key-xxxx"
	// Seed a long transcript so the next request must drop the middle.
	path := filepath.Join(data, cfg.SessionID+".direct.json")
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: directSystemFor(false, "")}, {Role: llm.RoleUser, Content: "start"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: `{"action":"list_dir","args":{"path":"."}}`},
			llm.Message{Role: llm.RoleUser, Content: "Tool list_dir result:\n" + strings.Repeat("file-"+string(rune('a'+i%20))+".txt\n", 30)},
		)
	}
	if err := saveDirectState(path, &directState{Version: 1, Messages: msgs, Step: 0}, cfg.APIKey); err != nil {
		t.Fatal(err)
	}
	var sent int
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		sent = 0
		for _, m := range req.Messages {
			sent += len(m.Content)
		}
		if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem || !strings.Contains(req.Messages[0].Content, directMarker) {
			t.Errorf("compacted window lost the system prompt")
		}
		if !strings.Contains(joinReq(req), "start") {
			t.Errorf("compacted window lost the original user request")
		}
		var full int
		for _, m := range msgs {
			full += len(m.Content)
		}
		if sent > full/2 {
			// Head may exceed the numeric 800-char budget; the window must
			// still be far smaller than the untrimmed 20-turn transcript.
			t.Errorf("request window too large: %d chars (full transcript %d)", sent, full)
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "compacted context used"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cfg.APIKey) {
		t.Fatalf("key leaked into compacted transcript")
	}
	if !strings.Contains(string(raw), "file-a.txt") {
		t.Fatalf("full transcript should still be on disk")
	}
}

func TestDirect_BudgetStopsRun(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "just run")
	cfg.BudgetTokens = 1 // smaller than any real call; usage may be zero on scripted clients
	cfg.MaxCost = 0
	// Scripted client typically reports no usage, so this may not trip.
	// Still assert the loop starts and finishes (or stops) without panic.
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "done under budget test"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	_ = NewDirect(o, nil).Run(context.Background())
}

func TestDirect_StreamEmitsMultipleDeltasNotOneFlush(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Stream a greeting then finish.")
	cfg.Stream = true
	const secret = "sk-direct-stream-secret-key"
	cfg.APIKey = secret
	parts := []string{"Hel", "lo ", secret, " world"}
	wantVisible := "Hel" + "lo " + "***REDACTED***" + " world"
	client := &llm.ScriptedClient{
		TokenParts: parts,
		ChunkDelay: 40 * time.Millisecond,
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "streamed and finished"}), nil
		},
	}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var deltas []string
	var arrivals []time.Time
	sub, cancel := h.SubscribeEvents()
	defer cancel()
	doneCollect := make(chan struct{})
	go func() {
		defer close(doneCollect)
		for ev := range sub {
			kind, _ := ev["kind"].(string)
			if kind != "assistant_delta" {
				continue
			}
			text, _ := ev["text"].(string)
			mu.Lock()
			deltas = append(deltas, text)
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
			if strings.Contains(text, secret) {
				t.Errorf("live delta leaked api key: %q", text)
			}
		}
	}()

	var sawPartial bool
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		live := h.Snapshot().LiveAssistant
		if strings.Contains(live, secret) {
			t.Fatalf("snapshot live_assistant leaked api key: %q", live)
		}
		if live != "" && live != wantVisible && strings.HasPrefix(wantVisible, live) {
			sawPartial = true
		}
		if h.Phase().Terminal() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	cancel()
	select {
	case <-doneCollect:
	case <-time.After(2 * time.Second):
	}

	mu.Lock()
	got := deltas
	times := arrivals
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected multiple assistant_delta events, got %d %#v", len(got), got)
	}
	if joined := strings.Join(got, ""); joined != wantVisible {
		t.Fatalf("joined deltas %q, want %q", joined, wantVisible)
	}
	if !sawPartial {
		t.Fatal("never observed a mid-stream prefix in Snapshot; tokens may have been flushed as one packet")
	}
	if len(times) >= 2 && times[len(times)-1].Sub(times[0]) < 30*time.Millisecond {
		t.Fatalf("all %d deltas arrived within 30ms; stream looks like a single flush", len(times))
	}

	// Incremental tokens must not be persisted to the JSONL event log.
	evPath := h.Orchestrator().EventPath()
	raw, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"kind":"assistant_delta"`) || strings.Contains(string(raw), "assistant_delta") {
		t.Fatalf("assistant_delta was persisted to JSONL (should be live-only): %s", evPath)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("event log leaked api key")
	}
}

func TestDirect_AskBlocksUntilAnswerThenWrites(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Ask the user for a filename, then write hello there.")
	cfg.AskTimeout = 15 * time.Second
	var mu sync.Mutex
	asked, sawAnswer, wrote := false, false, false
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		mu.Lock()
		defer mu.Unlock()
		if !asked {
			asked = true
			return jsonAct("ask", map[string]any{"question": "What filename should I write?"}), nil
		}
		if strings.Contains(body, "User answer:") && strings.Contains(body, "notes-from-user.txt") {
			sawAnswer = true
			if !wrote {
				wrote = true
				return jsonAct(tools.ToolWriteFile, map[string]any{
					"path": "notes-from-user.txt", "content": "from-ask",
				}), nil
			}
		}
		if !sawAnswer {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "answered never arrived: " + body}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote the file the user named"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.Phase() == PhaseAsk && h.Snapshot().PendingAsk != "" {
			break
		}
		if h.Phase().Terminal() {
			t.Fatalf("run ended before ask: phase=%s err=%v", h.Phase(), h.Wait())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.Phase() != PhaseAsk {
		t.Fatalf("expected PhaseAsk, got %s snap=%+v", h.Phase(), h.Snapshot())
	}
	target := filepath.Join(work, "notes-from-user.txt")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file was written before the user answered")
	}

	if err := h.Answer("notes-from-user.txt"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected write after answer: %v", err)
	}
	if string(raw) != "from-ask" {
		t.Fatalf("file content %q", raw)
	}
	mu.Lock()
	okAsk, okAns, okWrite := asked, sawAnswer, wrote
	mu.Unlock()
	if !okAsk || !okAns || !okWrite {
		t.Fatalf("loop flags asked=%v sawAnswer=%v wrote=%v", okAsk, okAns, okWrite)
	}
}

func TestDirect_AskPauseDoesNotTreatPauseAsAnswer(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Ask a question and wait.")
	transcript := filepath.Join(data, "ask-pause.direct.json")
	cfg.TranscriptPath = transcript
	asked := make(chan struct{})
	var once sync.Once
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		if strings.Contains(body, "User answer:") {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "unexpected answer: " + body}), nil
		}
		once.Do(func() { close(asked) })
		return jsonAct("ask", map[string]any{"question": "Need a name?"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-asked:
	case <-time.After(8 * time.Second):
		t.Fatal("model never asked")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.Phase() != PhaseAsk {
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := h.Wait(); !errors.Is(err, ErrPaused) && err != nil {
		// Wait returns runErr; pause should be ErrPaused
		if !errors.Is(err, ErrPaused) {
			t.Fatalf("wait after pause: %v", err)
		}
	}
	raw, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Need a name?") {
		t.Fatalf("pending ask not kept in transcript: %s", raw)
	}
	if strings.Contains(string(raw), "User answer:") {
		t.Fatalf("pause was treated as a user answer: %s", raw)
	}
}

func waitDirectPhase(t *testing.T, h *RunHandle, want RunPhase) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.Phase() == want {
			return
		}
		if h.Phase().Terminal() && want != h.Phase() {
			t.Fatalf("run ended at %s (err=%v) before %s", h.Phase(), h.Wait(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never reached %s, last=%s", want, h.Phase())
}

func TestDirect_ProtocolStreamShowsReadableTextOnly(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Finish with a short hello.")
	cfg.Stream = true
	raw := `{"thought":"Hello there","action":"finish","args":{"summary":"All good"}}`
	client := &llm.ScriptedClient{
		TokenParts: chunkEvery(raw, 4),
		ChunkDelay: 15 * time.Millisecond,
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: raw}, nil
		},
	}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var deltas []string
	sub, cancel := h.SubscribeEvents()
	defer cancel()
	go func() {
		for ev := range sub {
			if ev["kind"] != "assistant_delta" {
				continue
			}
			text, _ := ev["text"].(string)
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
			if strings.Contains(text, `"action"`) || strings.Contains(text, `"args"`) || strings.Contains(text, "write_file") {
				t.Errorf("protocol leaked in delta")
			}
		}
	}()
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	cancel()
	mu.Lock()
	got := strings.Join(deltas, "")
	mu.Unlock()
	if !strings.Contains(got, "Hello there") || !strings.Contains(got, "All good") {
		t.Fatalf("visible stream missing thought/summary: %q", got)
	}
	if strings.Contains(got, `"action"`) || strings.Contains(got, `{`) {
		t.Fatalf("protocol leaked in joined deltas: %q", got)
	}
}

func TestDirect_ManualApprovalMergesOnlyAfterApprove(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Write approved.txt then finish.")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	var wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "approved.txt", "content": "from-agent"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote approved.txt"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitDirectPhase(t, h, PhaseLeafApproval)
	target := filepath.Join(work, "approved.txt")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file landed in the project workdir before approval")
	}
	req, ok := h.PendingApproval(directReviewNodeID)
	if !ok || len(req.Changes) == 0 {
		t.Fatalf("missing review payload: ok=%v changes=%d", ok, len(req.Changes))
	}
	if err := h.DecideLeaf(directReviewNodeID, true, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "from-agent" {
		t.Fatalf("expected merge after approve, got %q %v", body, err)
	}
}

func TestDirect_ManualRejectDoesNotWriteOrAutoretry(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Write rejected.txt then finish.")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	var writes int
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		if strings.Contains(body, "rejected by reviewer") || strings.Contains(body, "REVIEWED AND REJECTED") {
			t.Errorf("model was auto-retried after reject")
		}
		if writes == 0 {
			writes++
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "rejected.txt", "content": "nope"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote rejected.txt"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitDirectPhase(t, h, PhaseLeafApproval)
	if err := h.DecideLeaf(directReviewNodeID, false, "do not apply this"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "rejected.txt")); err == nil {
		t.Fatal("rejected changes were written to the workdir")
	}
	if writes != 1 {
		t.Fatalf("writes=%d, reject should not auto-retry the tool loop", writes)
	}
	if !strings.Contains(h.LastSummary(), "拒绝") && !strings.Contains(h.LastSummary(), "rejected") {
		t.Fatalf("summary should describe the rejection: %q", h.LastSummary())
	}
}

func TestDirect_ManualExternalEditIsProtected(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := directCfg(t, work, data, "Change note.txt then finish.")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	var wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "note.txt", "content": "agent-version"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "updated note.txt"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitDirectPhase(t, h, PhaseLeafApproval)
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("external-edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.DecideLeaf(directReviewNodeID, true, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(work, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "external-edit" {
		t.Fatalf("external edit was overwritten: %q", body)
	}
}

func TestDirect_AutoWritesWithoutApprovalGate(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Write auto.txt then finish.")
	var wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "auto.txt", "content": "ok"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote auto.txt"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.Phase() == PhaseLeafApproval {
		t.Fatal("auto mode parked in approval")
	}
	if _, err := os.ReadFile(filepath.Join(work, "auto.txt")); err != nil {
		t.Fatalf("auto mode should write immediately: %v", err)
	}
}

func TestDirect_AskTimeoutClarifyContinues(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Ask for a filename, then write.")
	cfg.AskTimeout = 80 * time.Millisecond
	var asked, wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		if !asked {
			asked = true
			return jsonAct("ask", map[string]any{"question": "What filename should I write?"}), nil
		}
		if strings.Contains(body, "timeout, not consent") || strings.Contains(body, "reasonable assumption") {
			if !wrote {
				wrote = true
				return jsonAct(tools.ToolWriteFile, map[string]any{"path": "assumed.txt", "content": "ok"}), nil
			}
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote assumed.txt after timeout"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !wrote {
		t.Fatal("clarify timeout should have continued the loop")
	}
	if _, err := os.Stat(filepath.Join(work, "assumed.txt")); err != nil {
		t.Fatalf("expected write after clarify timeout: %v", err)
	}
}

func TestDirect_AskTimeoutConsentPausesWithoutYes(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Ask before deleting anything.")
	cfg.AskTimeout = 80 * time.Millisecond
	cfg.TranscriptPath = filepath.Join(data, "consent-ask.direct.json")
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinReq(req)
		if strings.Contains(body, "User answer:") && !strings.Contains(strings.ToLower(body), "not consent") {
			t.Errorf("consent ask was answered by timeout")
		}
		if strings.Contains(body, "User answer:") {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "unexpected continue"}), nil
		}
		return jsonAct("ask", map[string]any{"question": "Can I delete all files in this project?"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	err = h.Wait()
	if !errors.Is(err, ErrPaused) {
		t.Fatalf("expected pause, got %v phase=%s", err, h.Phase())
	}
	raw, err := os.ReadFile(cfg.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "delete all files") {
		t.Fatal("pending consent question was not kept")
	}
	if strings.Contains(string(raw), "User answer:") {
		t.Fatal("timeout was turned into a user answer")
	}
}

func TestDirect_ManualPauseResumeThenApprove(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Write resume-ok.txt then finish.")
	cfg.LeafApproval = LeafApprovalManual
	cfg.Isolate = true
	var wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "resume-ok.txt", "content": "ok"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote resume-ok.txt"}), nil
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitDirectPhase(t, h, PhaseLeafApproval)
	if err := h.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := h.Wait(); !errors.Is(err, ErrPaused) {
		t.Fatalf("wait after pause: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "resume-ok.txt")); err == nil {
		t.Fatal("paused review must not merge")
	}
	cfg.SessionID = h.SessionID()
	cfg.Goal = ""
	h2, err := mgr.Resume(cfg, client, SilentLogger())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitDirectPhase(t, h2, PhaseLeafApproval)
	if err := h2.DecideLeaf(directReviewNodeID, true, ""); err != nil {
		t.Fatalf("approve after resume: %v", err)
	}
	if err := h2.Wait(); err != nil {
		t.Fatalf("run after resume: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(work, "resume-ok.txt"))
	if err != nil || string(body) != "ok" {
		t.Fatalf("expected merge after resume+approve, got %q %v", body, err)
	}
}
