package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

func nativeTC(id, name string, args map[string]any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Type: "function", Function: llm.ToolFunction{Name: name, Arguments: string(b)}}
}

func nativeResp(calls ...llm.ToolCall) *llm.Response {
	return &llm.Response{ToolCalls: calls}
}

// TestDirect_ContextWindowKeepsSystemAndGoal reproduces the direct-loop
// compaction defect: tailWithinChars is applied to the whole transcript, so a
// tight budget drops the system prompt and the original user request. The
// model must still see both, plus a recent tail that does not orphan tool
// results. Elided early results stay on disk.
func TestDirect_ContextWindowKeepsSystemAndGoal(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "GOAL-KEEP-ME compact the window")
	cfg.NativeTools = true
	cfg.DirectContextChars = 4000
	path := filepath.Join(data, cfg.SessionID+".direct.json")

	const early = "EARLY-UNIQUE-aaaaaaaa"
	const late = "LATE-UNIQUE-zzzzzzzz"
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: "GOAL-KEEP-ME compact the window"},
	}
	for i := 0; i < 16; i++ {
		id := "call-" + itoa(i)
		marker := ""
		if i == 0 {
			marker = early + "\n"
		}
		if i == 15 {
			marker = late + "\n"
		}
		body := marker + strings.Repeat("blob-"+itoa(i)+"-xxxx\n", 40)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{nativeTC(id, tools.ToolReadFile, map[string]any{"path": "f" + itoa(i) + ".txt"})}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: tools.ToolReadFile, Content: body},
		)
	}
	if err := saveDirectState(path, &directState{Version: 1, Messages: msgs, Step: 0}, cfg.APIKey); err != nil {
		t.Fatal(err)
	}

	var saw bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		saw = true
		if len(req.Messages) == 0 {
			t.Fatal("empty request window")
		}
		if req.Messages[0].Role != llm.RoleSystem || !strings.Contains(req.Messages[0].Content, directMarker) {
			t.Errorf("compacted window lost the system prompt; first role=%q prefix=%q", req.Messages[0].Role, clip(req.Messages[0].Content, 80))
		}
		joined := joinReq(req)
		if !strings.Contains(joined, "GOAL-KEEP-ME") {
			t.Errorf("compacted window lost the original user request")
		}
		if strings.Contains(joined, early) {
			t.Errorf("early elided tool result was still sent to the model")
		}
		if !strings.Contains(joined, late) {
			t.Errorf("most recent tool result missing from the compacted window")
		}
		if orphans := orphanToolResults(req.Messages); len(orphans) > 0 {
			t.Errorf("compacted window orphaned tool_call ids: %v", orphans)
		}
		var sent int
		for _, m := range req.Messages {
			sent += messageChars(m)
		}
		if sent > 4000+len(directSystemFor(false, ""))+500 {
			// Head may legally exceed the numeric budget, but the window must
			// stay far smaller than the full 16-turn transcript (~tens of KB).
			t.Errorf("request window too large: %d chars", sent)
		}
		return nativeResp(nativeTC("fin", tools.ToolFinish, map[string]any{"summary": "window kept the head"})), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !saw {
		t.Fatal("model was not called")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), early) {
		t.Fatal("full transcript on disk should still contain the early tool result")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// TestDirect_SameTurnFailedCheckDoesNotFinish is the multi-tool protocol
// gap: a single native reply can emit run_bash (failing tests) and finish
// together. applyActions currently still honors finish because a non-zero
// exit is returned as text, not an error. The failing output must go back
// to the model, finish in that same reply must not complete the run, and
// the model must be able to repair and finish later.
func TestDirect_SameTurnFailedCheckDoesNotFinish(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Implement Add(a, b int) int in add.go with a test; go test must pass.")
	cfg.NativeTools = true
	drv := &sameTurnRepairDriver{}
	o, err := NewFromGoal(cfg, drv.client(), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !drv.sawFailedTest {
		t.Fatal("model never saw the failing go test output after the same-turn finish attempt")
	}
	if !drv.sawSkippedFinish {
		t.Fatal("the same-turn finish was not reported back as unexecuted; the run likely completed on the failed check")
	}
	if drv.finishedEarly {
		t.Fatal("model issued a later finish without being told the earlier same-turn finish was skipped")
	}
	if ctl.summary == "" || strings.Contains(strings.ToLower(ctl.summary), "all tests passed on the first try") {
		t.Fatalf("final summary must come from the post-repair finish, got %q", ctl.summary)
	}
	if !strings.Contains(ctl.summary, "subtraction") && !strings.Contains(ctl.summary, "addition") {
		t.Fatalf("final summary should describe the repair, got %q", ctl.summary)
	}
	out, err := runGoTest(t, work)
	if err != nil {
		t.Fatalf("workspace tests should pass after the repair: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FAIL") {
		t.Fatal("transcript should contain the real failing go test output")
	}
	if !strings.Contains(string(raw), "ok  ") && !strings.Contains(string(raw), "PASS") {
		t.Fatalf("transcript should contain a later passing go test")
	}
	st, err := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
	if err != nil || st == nil || !st.Done {
		t.Fatalf("transcript should be marked done after the real finish, st=%v err=%v", st, err)
	}
}

type sameTurnRepairDriver struct {
	mu                sync.Mutex
	wroteMod          bool
	wroteAdd          bool
	wroteTest         bool
	attemptedTogether bool
	sawFailedTest     bool
	sawSkippedFinish  bool
	wroteFix          bool
	ranPass           bool
	finishedEarly     bool
}

func (d *sameTurnRepairDriver) client() llm.Client {
	return &llm.ScriptedClient{Handle: d.handle}
}

func (d *sameTurnRepairDriver) handle(ctx context.Context, req llm.Request) (*llm.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if orphans := orphanToolResults(req.Messages); len(orphans) > 0 {
		return nativeResp(nativeTC("bad-proto", tools.ToolFinish, map[string]any{"summary": "orphaned tool ids: " + strings.Join(orphans, ",")})), nil
	}
	body := joinReq(req)
	last := lastToolResult(req)

	if !d.wroteMod {
		d.wroteMod = true
		return nativeResp(nativeTC("w-mod", tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod})), nil
	}
	if !d.wroteAdd {
		d.wroteAdd = true
		return nativeResp(nativeTC("w-add", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd})), nil
	}
	if !d.wroteTest {
		d.wroteTest = true
		return nativeResp(nativeTC("w-test", tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest})), nil
	}
	if !d.attemptedTogether {
		d.attemptedTogether = true
		return nativeResp(
			nativeTC("t-fail", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}),
			nativeTC("f-early", tools.ToolFinish, map[string]any{"summary": "all tests passed on the first try"}),
		), nil
	}
	if strings.Contains(body, "FAIL") || strings.Contains(last, "FAIL") {
		d.sawFailedTest = true
	}
	if strings.Contains(strings.ToLower(body+last), "not executed") ||
		strings.Contains(strings.ToLower(body+last), "already called finish") ||
		strings.Contains(body, "f-early") && strings.Contains(strings.ToLower(body+last), "fail") {
		// A tool result bound to the early finish id, or an explicit skip
		// reason, means the same-turn finish was not treated as success.
		if strings.Contains(body, "Not executed") || strings.Contains(last, "Not executed") ||
			strings.Contains(strings.ToLower(body+last), "failed") ||
			toolResultFor(req, "f-early") != "" && !strings.HasPrefix(strings.ToLower(toolResultFor(req, "f-early")), "finished:") {
			d.sawSkippedFinish = true
		}
	}
	if toolRes := toolResultFor(req, "f-early"); toolRes != "" {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(toolRes)), "finished:") {
			d.finishedEarly = true
		} else {
			d.sawSkippedFinish = true
		}
	}

	if !d.wroteFix {
		if !d.sawFailedTest {
			return nativeResp(nativeTC("f-no-fail", tools.ToolFinish, map[string]any{"summary": "expected failing test output, last=" + last})), nil
		}
		d.wroteFix = true
		return nativeResp(nativeTC("w-fix", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd})), nil
	}
	if !d.ranPass {
		d.ranPass = true
		return nativeResp(nativeTC("t-pass", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60})), nil
	}
	if !strings.Contains(last, "ok  ") && !strings.Contains(last, "PASS") && !strings.Contains(body, "ok  ") {
		return nativeResp(nativeTC("f-still-fail", tools.ToolFinish, map[string]any{"summary": "expected passing test after the fix, last=" + last})), nil
	}
	return nativeResp(nativeTC("f-done", tools.ToolFinish, map[string]any{
		"summary": "The first go test failed because Add used subtraction; I changed it to addition and go test passed.",
	})), nil
}

func toolResultFor(req llm.Request, id string) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// TestDirect_HTTPProtocolRepairAndBudget uses a local OpenAI-compatible
// /chat/completions server (not a fixed final-text mock) to drive the real
// client. It covers: tool_call ids echoed back, a recoverable tool error,
// a same-turn failed check that must not complete the run, a later repair,
// and a token budget that must not be reported as a successful finish.
func TestDirect_HTTPProtocolRepairAndBudget(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var mu sync.Mutex
	n := 0
	var lastReq struct {
		Messages []llm.Message `json:"messages"`
		Tools    []llm.Tool    `json:"tools"`
	}

	writeJSON := func(w http.ResponseWriter, calls []llm.ToolCall, usage llm.Usage) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":       "assistant",
					"tool_calls": calls,
				},
			}},
			"usage": usage,
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&lastReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n++
		usage := llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}
		switch n {
		case 1:
			// Recoverable tool error first: read a path that does not exist.
			writeJSON(w, []llm.ToolCall{nativeTC("r-miss", tools.ToolReadFile, map[string]any{"path": "missing-no-such.go"})}, usage)
		case 2:
			if toolResultFor(llm.Request{Messages: lastReq.Messages}, "r-miss") == "" && !strings.Contains(joinReq(llm.Request{Messages: lastReq.Messages}), "missing-no-such.go") {
				writeJSON(w, []llm.ToolCall{nativeTC("f-no-err", tools.ToolFinish, map[string]any{"summary": "missing file error was not returned"})}, usage)
				return
			}
			writeJSON(w, []llm.ToolCall{
				nativeTC("w-mod", tools.ToolWriteFile, map[string]any{"path": "go.mod", "content": addMod}),
			}, usage)
		case 3:
			writeJSON(w, []llm.ToolCall{nativeTC("w-add", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd})}, usage)
		case 4:
			writeJSON(w, []llm.ToolCall{nativeTC("w-test", tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": addTest})}, usage)
		case 5:
			writeJSON(w, []llm.ToolCall{
				nativeTC("t-fail", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}),
				nativeTC("f-early", tools.ToolFinish, map[string]any{"summary": "all tests passed on the first try"}),
			}, usage)
		case 6:
			req := llm.Request{Messages: lastReq.Messages}
			if orphans := orphanToolResults(req.Messages); len(orphans) > 0 {
				writeJSON(w, []llm.ToolCall{nativeTC("f-orphan", tools.ToolFinish, map[string]any{"summary": "orphans " + strings.Join(orphans, ",")})}, usage)
				return
			}
			failOut := toolResultFor(req, "t-fail") + lastToolResult(req)
			finOut := toolResultFor(req, "f-early")
			if !strings.Contains(failOut, "FAIL") {
				writeJSON(w, []llm.ToolCall{nativeTC("f-no-fail", tools.ToolFinish, map[string]any{"summary": "failed test not in history"})}, usage)
				return
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(finOut)), "finished:") {
				writeJSON(w, []llm.ToolCall{nativeTC("f-early-honored", tools.ToolFinish, map[string]any{"summary": "same-turn finish was honored"})}, usage)
				return
			}
			writeJSON(w, []llm.ToolCall{nativeTC("w-fix", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd})}, usage)
		case 7:
			writeJSON(w, []llm.ToolCall{nativeTC("t-pass", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60})}, usage)
		default:
			writeJSON(w, []llm.ToolCall{nativeTC("f-done", tools.ToolFinish, map[string]any{
				"summary": "The first go test failed because Add used subtraction; I changed it to addition and go test passed.",
			})}, usage)
		}
	}))
	t.Cleanup(srv.Close)

	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)

	cfg := directCfg(t, work, data, "Implement Add(a, b int) int in add.go with a test; go test must pass.")
	cfg.NativeTools = true
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("repair run: %v", err)
	}
	if strings.Contains(strings.ToLower(ctl.summary), "all tests passed on the first try") ||
		strings.Contains(strings.ToLower(ctl.summary), "same-turn finish was honored") ||
		strings.Contains(strings.ToLower(ctl.summary), "failed test not in history") ||
		strings.Contains(strings.ToLower(ctl.summary), "missing file error was not returned") {
		t.Fatalf("repair run ended on a protocol failure summary: %q", ctl.summary)
	}
	if !strings.Contains(ctl.summary, "subtraction") {
		t.Fatalf("expected post-repair summary, got %q", ctl.summary)
	}
	if out, err := runGoTest(t, work); err != nil {
		t.Fatalf("tests should pass: %v\n%s", err, out)
	}

	// Fresh run: token budget must not complete as success even if the
	// model replies with finish. Usage is reported by the local server.
	work2, data2 := t.TempDir(), t.TempDir()
	n = 0
	srvBudget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"thought":"scripted","action":"finish","args":{"summary":"finished under budget"}}`,
				},
			}},
			"usage": llm.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
		})
	}))
	t.Cleanup(srvBudget.Close)
	cfg2 := directCfg(t, work2, data2, "just finish")
	cfg2.BudgetTokens = 50
	cfg2.SessionID = "sess_direct_budget_http"
	client2 := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srvBudget.URL, 0)
	o2, err := NewFromGoal(cfg2, client2, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o2.Close)
	err = NewDirect(o2, nil).Run(context.Background())
	if err == nil {
		t.Fatal("token budget must stop the run with an error, not a successful finish")
	}
	if errors.Is(err, ErrMaxSteps) {
		t.Fatalf("budget should cancel, not exhaust steps: %v", err)
	}
	st, loadErr := loadDirectState(filepath.Join(data2, o2.SessionID()+".direct.json"), cfg2.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st != nil && st.Done {
		t.Fatal("budget stop must not mark the transcript done")
	}
	if root := o2.tree.GetRoot(); root != nil && root.State == models.TaskStateCompleted {
		t.Fatal("budget stop must not mark the root completed")
	}
}

// TestDirect_InvalidJSONStopsBeforeBudget covers the weak-model JSON loop:
// three unparseable replies must end the turn with ErrInvalidJSON instead of
// burning every remaining step on "please reply with JSON".
func TestDirect_InvalidJSONStopsBeforeBudget(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "make a change")
	cfg.DirectSteps = 20
	calls := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		calls++
		return &llm.Response{Content: "this is not a tool call"}, nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, &testDirectCtl{}).Run(context.Background())
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("err = %v, want ErrInvalidJSON", err)
	}
	if calls != maxInvalidJSONRounds {
		t.Fatalf("model calls = %d, want %d (must not burn DirectSteps)", calls, maxInvalidJSONRounds)
	}
	st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st != nil && st.Done {
		t.Fatal("invalid-JSON abort must not mark the transcript done")
	}
	if root := o.tree.GetRoot(); root != nil && root.State == models.TaskStateCompleted {
		t.Fatal("invalid-JSON abort must not mark the root completed")
	}
}

// TestDirect_FinishGateRejectsConflictMarker covers the missing deterministic
// pre-finish review in direct mode: finish must be refused while an unresolved
// conflict marker is present, the model must see the report, and a later clean
// finish must succeed.
func TestDirect_FinishGateRejectsConflictMarker(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	initGitRepo(t, work)
	cfg := directCfg(t, work, data, "write a clean file")
	calls := 0
	wroteClean := false
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		calls++
		body := joinReq(req)
		switch {
		case calls == 1:
			return jsonAct(tools.ToolWriteFile, map[string]any{
				"path":    "broken.go",
				"content": "package demo\n\n<<<<<<< HEAD\nfunc A() {}\n=======\nfunc A() int { return 1 }\n>>>>>>> other\n",
			}), nil
		case calls == 2:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "should be blocked"}), nil
		case !wroteClean && strings.Contains(body, "REJECTED by review_diff"):
			wroteClean = true
			return jsonAct(tools.ToolWriteFile, map[string]any{
				"path":    "broken.go",
				"content": "package demo\n\nfunc A() int { return 1 }\n",
			}), nil
		default:
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "clean finish"}), nil
		}
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ctl.summary != "clean finish" {
		t.Fatalf("summary = %q, want the post-repair finish (gate did not block or was not retried)", ctl.summary)
	}
	raw, err := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "REJECTED by review_diff") {
		t.Fatal("transcript should show the rejected finish and its review report")
	}
}

// TestDirect_RepeatedIdenticalCallGetsIntervention covers the other weak-model
// loop shape: the same successful read repeated until the budget runs out. The
// model must be told the repetition cannot help, and the turn must still be
// able to finish.
func TestDirect_RepeatedIdenticalCallGetsIntervention(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := directCfg(t, work, data, "read note.txt, then finish")
	cfg.DirectSteps = 10
	calls := 0
	sawIntervention := false
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		calls++
		body := joinReq(req)
		if calls <= directRepeatLimit {
			return jsonAct(tools.ToolReadFile, map[string]any{"path": "note.txt"}), nil
		}
		if strings.Contains(body, "identical arguments") {
			sawIntervention = true
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "stopped repeating"}), nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sawIntervention {
		t.Fatal("model never saw the repeated-call intervention")
	}
	if ctl.summary != "stopped repeating" {
		t.Fatalf("summary = %q", ctl.summary)
	}
	raw, err := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "identical arguments") {
		t.Fatal("transcript should keep the intervention")
	}
}

// TestDirect_StallEndsTurnAfterInterventions is the observed weak-model run:
// the model re-runs the same failing verification forever. Two observation
// reminders are the conversational budget; after that, directFailureLimit
// further identical failures must end the turn with ErrStall instead of
// quietly spending the remaining step budget (the real run burned ten more
// calls after the last reminder, then died on the request timeout).
func TestDirect_StallEndsTurnAfterInterventions(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "make the test pass")
	cfg.DirectSteps = 40
	calls := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		calls++
		return &llm.Response{Content: `{"thought":"retry","action":"run_bash","args":{"command":"go test ./..."}}`}, nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	err = NewDirect(o, &testDirectCtl{}).Run(context.Background())
	if !errors.Is(err, ErrStall) {
		t.Fatalf("err = %v, want ErrStall", err)
	}
	want := directFailureLimit * (directFailureNags + 1)
	if calls != want {
		t.Fatalf("model calls = %d, want %d (two reminders, then the turn must end)", calls, want)
	}
	root := o.tree.GetRoot()
	if root == nil || root.State != models.TaskStateFailed {
		t.Fatalf("root must be failed, got %+v", root)
	}
	if !strings.Contains(root.ErrorMsg, "go test ./...") || !strings.Contains(root.ErrorMsg, "observation reminders") {
		t.Fatalf("root error should name the command and the reminders: %q", root.ErrorMsg)
	}
	st, loadErr := loadDirectState(filepath.Join(data, o.SessionID()+".direct.json"), cfg.APIKey)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st != nil && st.Done {
		t.Fatal("stall stop must not mark the transcript done")
	}
}

// TestDirect_FirstRequestToleratesColdModelLoad is the observed Ollama
// behavior: after ~5 idle minutes the model is evicted, and the run's first
// request spends that load time instead of generating — which used to burn
// RequestTimeout and kill the turn before its first real step. The first
// request must get the cold-start floor; later requests stay on
// RequestTimeout.

// TestDirect_ContextWindowKeepsLaterFollowUps is the long-session follow-up
// gap: directRequestWindow pins only the system prompt and the first user
// message. A mid-conversation inject or later follow-up sits in the body and
// is dropped when a tight budget keeps a tool-heavy tail. Continuous coding
// must still see that later instruction, along with the original goal.
func TestDirect_ContextWindowKeepsLaterFollowUps(t *testing.T) {
	const follow = "FOLLOW-UP-KEEP-ME rename the package"
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: "The user's request for this turn:\nGOAL-KEEP-ME implement Add"},
	}
	// A couple of early tool turns, then the inject, then a long tail that
	// fills the budget so the follow-up would be evicted today.
	for i := 0; i < 2; i++ {
		id := "early-" + itoa(i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{nativeTC(id, tools.ToolReadFile, map[string]any{"path": "e" + itoa(i) + ".txt"})}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: tools.ToolReadFile, Content: "EARLY-BLOB-" + itoa(i) + "\n" + strings.Repeat("x", 200)},
		)
	}
	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: "User follow-up (inject at the next model step; do not restart the whole task). Honor this from now on:\n" + follow,
	})
	const late = "LATE-UNIQUE-zzzzzzzz"
	for i := 0; i < 16; i++ {
		id := "late-" + itoa(i)
		body := strings.Repeat("blob-"+itoa(i)+"-yyyy\n", 40)
		if i == 15 {
			body = late + "\n" + body
		}
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{nativeTC(id, tools.ToolReadFile, map[string]any{"path": "l" + itoa(i) + ".txt"})}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: tools.ToolReadFile, Content: body},
		)
	}

	window, dropped := directRequestWindow(msgs, 4000)
	if dropped == 0 {
		t.Fatal("expected the tight budget to drop some early tool turns")
	}
	joined := joinReq(llm.Request{Messages: window})
	if !strings.Contains(joined, "GOAL-KEEP-ME") {
		t.Errorf("window lost the original user request")
	}
	if !strings.Contains(joined, follow) {
		t.Errorf("window lost the later user follow-up; this is the continuous-coding gap")
	}
	if !strings.Contains(joined, late) {
		t.Errorf("window lost the most recent tool result")
	}
	if strings.Contains(joined, "EARLY-BLOB-0") {
		t.Errorf("an early elided tool result was still sent; budget should prefer later tools plus pinned follow-ups")
	}
	if orphans := orphanToolResults(window); len(orphans) > 0 {
		t.Errorf("window orphaned tool_call ids: %v", orphans)
	}
}

const earlyAddBodySecret = "ADD-BODY-SECRET-do-not-resend"
const earlyFailMarker = "EARLY-FAIL-MARKER-TestAdd"

func jsonToolPair(name string, args map[string]any, result string) []llm.Message {
	payload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": name, "args": args})
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: string(payload)},
		{Role: llm.RoleUser, Content: fmt.Sprintf("Tool %s result:\n%s", name, result)},
	}
}

func nativeToolPair(id, name string, args map[string]any, result string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{nativeTC(id, name, args)}},
		{Role: llm.RoleTool, ToolCallID: id, Name: name, Content: result},
	}
}

// workMemoryHistory is a long direct transcript: read and edit add.go, a
// failing go test later superseded by a passing one, a mid-session follow-up,
// then a tool-heavy tail that fills a tight budget so the early work is elided.
func workMemoryHistory(native bool, keepPassInTail bool) []llm.Message {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: "The user's request for this turn:\nGOAL-KEEP-ME implement Add"},
	}
	pair := nativeToolPair
	if !native {
		pair = func(id, name string, args map[string]any, result string) []llm.Message {
			return jsonToolPair(name, args, result)
		}
	}
	msgs = append(msgs, pair("r-add", tools.ToolReadFile, map[string]any{"path": "add.go"}, "package addtest\n"+earlyAddBodySecret+"\n")...)
	msgs = append(msgs, pair("w-add-bug", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyAdd}, "wrote add.go")...)
	msgs = append(msgs, pair("t-fail", tools.ToolRunBash, map[string]any{"command": "go test ."}, "exit_code: 1\n"+earlyFailMarker+"\nFAIL\n")...)
	msgs = append(msgs, pair("w-add-fix", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedAdd}, "wrote add.go")...)
	passOut := "exit_code: 0\nok  \taddtest\n"
	if !keepPassInTail {
		msgs = append(msgs, pair("t-pass", tools.ToolRunBash, map[string]any{"command": "go test ."}, passOut)...)
	}
	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: "User follow-up (inject at the next model step; do not restart the whole task). Honor this from now on:\nFOLLOW-UP-KEEP-ME also implement Mul",
	})
	const late = "LATE-UNIQUE-zzzzzzzz"
	for i := 0; i < 16; i++ {
		body := strings.Repeat("blob-"+itoa(i)+"-yyyy\n", 40)
		if i == 15 {
			body = late + "\n" + body
		}
		msgs = append(msgs, pair("late-"+itoa(i), tools.ToolReadFile, map[string]any{"path": "pad-" + itoa(i) + ".txt"}, body)...)
	}
	if keepPassInTail {
		msgs = append(msgs, pair("t-pass-tail", tools.ToolRunBash, map[string]any{"command": "go test ."}, passOut+late+"\n")...)
	}
	return msgs
}

func assertWorkMemoryWindow(t *testing.T, window []llm.Message, dropped int, wantPassInMemory bool) {
	t.Helper()
	if dropped == 0 {
		t.Fatal("expected the tight budget to drop early tool turns")
	}
	joined := joinReq(llm.Request{Messages: window})
	if !strings.Contains(joined, "GOAL-KEEP-ME") {
		t.Errorf("window lost the original user request")
	}
	if !strings.Contains(joined, "FOLLOW-UP-KEEP-ME") {
		t.Errorf("window lost the later user follow-up")
	}
	if !strings.Contains(joined, "LATE-UNIQUE-zzzzzzzz") {
		t.Errorf("window lost the most recent tool result")
	}
	if strings.Contains(joined, earlyAddBodySecret) {
		t.Errorf("omitted file contents were resent as if current; memory must not quote stale bodies")
	}
	if strings.Contains(joined, earlyFailMarker) {
		t.Errorf("stale failing test output was resent; a later success must supersede it")
	}
	if !strings.Contains(joined, "add.go") {
		t.Errorf("compacted window lost the evidence that add.go was already read or edited")
	}
	if wantPassInMemory {
		if !strings.Contains(joined, "go test .") {
			t.Errorf("compacted window lost the last observed go test result")
		}
		// Latest omitted outcome is exit 0. A memory that only keeps the
		// earlier failure (or invents nothing) is the amnesia/staleness bug.
		if !strings.Contains(joined, "exit 0") && !strings.Contains(joined, "exit_code: 0") {
			t.Errorf("last observed go test success is missing from working memory; got window:\n%s", clip(joined, 2000))
		}
	} else if mem := workMemoryMessage(window); mem != "" && (strings.Contains(mem, "exit 1") || strings.Contains(mem, "exit_code: 1")) && strings.Contains(mem, "go test") {
		t.Errorf("working memory still reports a dropped go test failure after a later passing result remains in the tail:\n%s", mem)
	}
	if orphans := orphanToolResults(window); len(orphans) > 0 {
		t.Errorf("window orphaned tool_call ids: %v", orphans)
	}
	mem := workMemoryMessage(window)
	if mem == "" {
		t.Errorf("no working-memory message was added for the omitted turns")
		return
	}
	if strings.HasPrefix(strings.TrimSpace(mem), "System:") || strings.Contains(strings.ToLower(mem), "you must now") {
		t.Errorf("working memory must stay historical data, not a system instruction")
	}
}

func workMemoryMessage(window []llm.Message) string {
	for _, m := range window {
		if m.Role != llm.RoleUser {
			continue
		}
		if strings.Contains(m.Content, "GOAL-KEEP-ME") || strings.Contains(m.Content, "FOLLOW-UP-KEEP-ME") {
			continue
		}
		if strings.Contains(m.Content, "Tool ") && strings.Contains(m.Content, "result:") {
			continue
		}
		if strings.Contains(m.Content, "omitted") || strings.Contains(m.Content, "working memory") || strings.Contains(m.Content, "elided") {
			return m.Content
		}
	}
	return ""
}

// TestDirect_ContextWindowKeepsWorkMemory is the long-task memory gap:
// directRequestWindow currently inserts only a count of omitted messages, so
// a tight budget drops the evidence of files already read or edited and of
// the last test outcome. The request must carry a bounded historical digest
// rebuilt from the transcript, without stale file bodies or an old failure
// covering a later success.
func TestDirect_ContextWindowKeepsWorkMemory(t *testing.T) {
	for _, native := range []bool{true, false} {
		name := "json"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			msgs := workMemoryHistory(native, false)
			window, dropped := directRequestWindow(msgs, 4000)
			assertWorkMemoryWindow(t, window, dropped, true)

			again, dropped2 := directRequestWindow(msgs, 4000)
			if dropped2 != dropped {
				t.Errorf("rebuilding the window changed dropped count %d -> %d", dropped, dropped2)
			}
			if a, b := joinReq(llm.Request{Messages: window}), joinReq(llm.Request{Messages: again}); a != b {
				t.Errorf("repeated compaction was not stable")
			}
			// If a previous digest were persisted, compacting again must not
			// nest copies without bound.
			if mem := workMemoryMessage(window); mem != "" {
				grown := append([]llm.Message{}, msgs[:2]...)
				grown = append(grown, llm.Message{Role: llm.RoleUser, Content: mem})
				grown = append(grown, msgs[2:]...)
				w2, _ := directRequestWindow(grown, 4000)
				if strings.Count(joinReq(llm.Request{Messages: w2}), mem[:min(40, len(mem))]) > 1 && len(workMemoryMessage(w2)) > len(mem)+200 {
					t.Errorf("persisted digest inflated on re-compaction: %d -> %d chars", len(mem), len(workMemoryMessage(w2)))
				}
			}
		})
		t.Run(name+"_passInTail", func(t *testing.T) {
			msgs := workMemoryHistory(native, true)
			window, dropped := directRequestWindow(msgs, 4000)
			assertWorkMemoryWindow(t, window, dropped, false)
		})
	}
}

func TestDirect_WorkMemoryIsBounded(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: directSystemFor(false, "")},
		{Role: llm.RoleUser, Content: "The user's request for this turn:\nGOAL-KEEP-ME many files"},
	}
	for i := 0; i < 80; i++ {
		path := "f" + itoa(i) + ".txt"
		msgs = append(msgs, nativeToolPair("r-"+itoa(i), tools.ToolReadFile, map[string]any{"path": path}, strings.Repeat("x", 200))...)
		msgs = append(msgs, nativeToolPair("w-"+itoa(i), tools.ToolWriteFile, map[string]any{"path": path, "content": "y"}, "wrote")...)
		msgs = append(msgs, nativeToolPair("t-"+itoa(i), tools.ToolRunBash, map[string]any{"command": "echo " + itoa(i)}, "exit_code: 0\n")...)
	}
	// Tail filler so the early unique mass is actually dropped.
	for i := 0; i < 8; i++ {
		msgs = append(msgs, nativeToolPair("pad-"+itoa(i), tools.ToolReadFile, map[string]any{"path": "pad-" + itoa(i) + ".bin"}, strings.Repeat("z", 800))...)
	}
	window, dropped := directRequestWindow(msgs, 4000)
	if dropped == 0 {
		t.Fatal("expected dropped history")
	}
	mem := workMemoryMessage(window)
	if mem == "" {
		t.Fatal("expected a working-memory digest for omitted turns")
	}
	if !strings.Contains(mem, ".txt") {
		t.Fatalf("working memory listed no omitted paths:\n%s", clip(mem, 800))
	}
	if len(mem) > 3500 {
		t.Errorf("working memory has no capacity bound: %d chars", len(mem))
	}
	if strings.Count(mem, ".txt") > 60 {
		t.Errorf("working memory listed too many paths: %q", clip(mem, 800))
	}
}

// TestDirect_HTTPWorkMemoryThenRepair resumes a saved long transcript over a
// local OpenAI-compatible server. The first request must include compact
// working memory and the new follow-up instruction (not the omitted file
// body). The scripted model then continues: edit, failing test, repair, pass,
// finish. A token-budget stop must not be marked success — covered by
// TestDirect_HTTPProtocolRepairAndBudget and not re-asserted here beyond the
// follow-up run completing only after a real passing test.
func TestDirect_HTTPWorkMemoryThenRepair(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(addMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "add.go"), []byte(fixedAdd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "add_test.go"), []byte(addTest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := directCfg(t, work, data, "Implement Mul(a, b int) int in add.go with a test; go test must pass.")
	cfg.NativeTools = true
	cfg.DirectContextChars = 4000
	path := filepath.Join(data, cfg.SessionID+".direct.json")
	msgs := workMemoryHistory(true, false)
	if err := saveDirectState(path, &directState{Version: 1, Messages: msgs, Step: 8, Done: true, Summary: "Add works"}, cfg.APIKey); err != nil {
		t.Fatal(err)
	}

	const buggyMul = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Mul(a, b int) int {\n\treturn a + b\n}\n"
	const fixedMul = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n"
	const mulTestSrc = "package addtest\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d\", got)\n\t}\n}\n\nfunc TestMul(t *testing.T) {\n\tif got := Mul(3, 4); got != 12 {\n\t\tt.Fatalf(\"Mul(3,4)=%d\", got)\n\t}\n}\n"

	var mu sync.Mutex
	n := 0
	var firstJoin string
	var sawMemory, sawFollow bool
	writeJSON := func(w http.ResponseWriter, calls []llm.ToolCall, usage llm.Usage) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":       "assistant",
					"tool_calls": calls,
				},
			}},
			"usage": usage,
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req llm.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		usage := llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}
		joined := joinReq(req)
		if n == 1 {
			firstJoin = joined
			if strings.Contains(joined, "add.go") && (strings.Contains(joined, "exit 0") || strings.Contains(joined, "exit_code: 0")) {
				sawMemory = true
			}
			if strings.Contains(joined, "Mul") || strings.Contains(joined, "New user request") {
				sawFollow = true
			}
			if strings.Contains(joined, earlyAddBodySecret) || strings.Contains(joined, earlyFailMarker) {
				writeJSON(w, []llm.ToolCall{nativeTC("f-stale", tools.ToolFinish, map[string]any{"summary": "stale omitted content was resent"})}, usage)
				return
			}
			if orphans := orphanToolResults(req.Messages); len(orphans) > 0 {
				writeJSON(w, []llm.ToolCall{nativeTC("f-orphan", tools.ToolFinish, map[string]any{"summary": "orphans " + strings.Join(orphans, ",")})}, usage)
				return
			}
			if !sawMemory || !sawFollow {
				writeJSON(w, []llm.ToolCall{nativeTC("f-no-mem", tools.ToolFinish, map[string]any{"summary": "first request missing working memory or follow-up"})}, usage)
				return
			}
			writeJSON(w, []llm.ToolCall{nativeTC("w-mul", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": buggyMul})}, usage)
			return
		}
		switch n {
		case 2:
			writeJSON(w, []llm.ToolCall{nativeTC("w-test", tools.ToolWriteFile, map[string]any{"path": "add_test.go", "content": mulTestSrc})}, usage)
		case 3:
			writeJSON(w, []llm.ToolCall{
				nativeTC("t-fail", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60}),
				nativeTC("f-early", tools.ToolFinish, map[string]any{"summary": "all tests passed on the first try"}),
			}, usage)
		case 4:
			failOut := toolResultFor(req, "t-fail") + lastToolResult(req)
			finOut := toolResultFor(req, "f-early")
			if !strings.Contains(failOut, "FAIL") {
				writeJSON(w, []llm.ToolCall{nativeTC("f-no-fail", tools.ToolFinish, map[string]any{"summary": "failed test not returned to the model"})}, usage)
				return
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(finOut)), "finished:") {
				writeJSON(w, []llm.ToolCall{nativeTC("f-early-honored", tools.ToolFinish, map[string]any{"summary": "same-turn finish was honored"})}, usage)
				return
			}
			writeJSON(w, []llm.ToolCall{nativeTC("w-fix", tools.ToolWriteFile, map[string]any{"path": "add.go", "content": fixedMul})}, usage)
		case 5:
			writeJSON(w, []llm.ToolCall{nativeTC("t-pass", tools.ToolRunBash, map[string]any{"command": "go test .", "timeout_sec": 60})}, usage)
		default:
			writeJSON(w, []llm.ToolCall{nativeTC("f-done", tools.ToolFinish, map[string]any{
				"summary": "Mul used addition at first; I changed it to multiplication and go test passed.",
			})}, usage)
		}
	}))
	t.Cleanup(srv.Close)

	client := llm.NewOpenAIClient("sk-scripted-local-not-a-real-key", srv.URL, 0)
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	ctl := &testDirectCtl{}
	if err := NewDirect(o, ctl).Run(context.Background()); err != nil {
		t.Fatalf("follow-up run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawMemory || !sawFollow {
		t.Fatalf("first request missing working memory or follow-up instruction; window=%s", clip(firstJoin, 2000))
	}
	if strings.Contains(strings.ToLower(ctl.summary), "first request missing") ||
		strings.Contains(strings.ToLower(ctl.summary), "stale omitted") ||
		strings.Contains(strings.ToLower(ctl.summary), "all tests passed on the first try") ||
		strings.Contains(strings.ToLower(ctl.summary), "same-turn finish was honored") ||
		strings.Contains(strings.ToLower(ctl.summary), "failed test not returned") {
		t.Fatalf("run ended on a protocol failure summary: %q", ctl.summary)
	}
	if !strings.Contains(ctl.summary, "multiplication") && !strings.Contains(ctl.summary, "addition") {
		t.Fatalf("expected a post-repair summary, got %q", ctl.summary)
	}
	if out, err := runGoTest(t, work); err != nil {
		t.Fatalf("tests should pass after repair: %v\n%s", err, out)
	}
}

func assertDirectToolPairs(t *testing.T, msgs []llm.Message) {
	t.Helper()
	if orphans := orphanToolResults(msgs); len(orphans) > 0 {
		t.Errorf("window orphaned tool_call ids: %v", orphans)
	}
	pending := map[string]bool{}
	for i, m := range msgs {
		if len(pending) > 0 && m.Role != llm.RoleTool {
			t.Errorf("message %d interrupts pending tool group: %v", i, pending)
		}
		if m.Role == llm.RoleTool {
			if m.ToolCallID != "" && !pending[m.ToolCallID] {
				t.Errorf("message %d unexpected tool id %q", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
		for _, tc := range m.ToolCalls {
			pending[tc.ID] = true
		}
	}
	if len(pending) > 0 {
		t.Errorf("unanswered tool calls %v", pending)
	}
}

func findWorkMemory(msgs []llm.Message) string {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, directWorkMemoryPrefix) {
			return m.Content
		}
	}
	return ""
}

func windowChars(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageChars(m)
	}
	return n
}

func waitPhaseAsk(t *testing.T, h *RunHandle) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.Phase() == PhaseAsk && h.Snapshot().PendingAsk != "" {
			return
		}
		if h.Phase().Terminal() {
			t.Fatalf("run ended before ask: phase=%s err=%v", h.Phase(), h.Wait())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PhaseAsk, phase=%s snap=%+v", h.Phase(), h.Snapshot())
}

// TestDirect_AskAnswerSurvivesCompactionInRealLoop is C1: after a real ask
// (native or JSON) and a user answer, later large reads must not drop the
// answer's meaning from the next model request, and tool pairs must stay intact.
func TestDirect_AskAnswerSurvivesCompactionInRealLoop(t *testing.T) {
	const answer = "Use public function name Sum and keep the dependency list unchanged."
	for _, native := range []bool{true, false} {
		name := "json"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			work, data := t.TempDir(), t.TempDir()
			for i := 0; i < 16; i++ {
				if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("module%d.go", i)), []byte(strings.Repeat("package module\n", 120)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := directCfg(t, work, data, "Implement the new helper after clarifying its public API.")
			cfg.SessionID = "sess_direct_ask_answer_" + name
			cfg.NativeTools = native
			cfg.DirectContextChars = 4000
			cfg.DirectSteps = 30
			cfg.AskTimeout = 15 * time.Second

			var mu sync.Mutex
			step := 0
			var laterJoin string
			var sawAnswer bool
			client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
				assertDirectToolPairs(t, req.Messages)
				joined := joinReq(req)
				mu.Lock()
				defer mu.Unlock()
				step++
				if step == 1 {
					if native {
						return nativeResp(nativeTC("ask-name", "ask", map[string]any{"question": "Which name and dependency policy?"})), nil
					}
					return jsonAct("ask", map[string]any{"question": "Which name and dependency policy?"}), nil
				}
				if strings.Contains(joined, answer) {
					sawAnswer = true
				}
				if step-2 < 16 {
					path := fmt.Sprintf("module%d.go", step-2)
					if native {
						return nativeResp(nativeTC("r-"+itoa(step), tools.ToolReadFile, map[string]any{"path": path})), nil
					}
					return jsonAct(tools.ToolReadFile, map[string]any{"path": path}), nil
				}
				laterJoin = joined
				return jsonAct(tools.ToolFinish, map[string]any{"summary": "clarified the API and inspected the modules"}), nil
			}}

			mgr := NewRunManager()
			t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
			h, err := mgr.Start(cfg, client, SilentLogger())
			if err != nil {
				t.Fatal(err)
			}
			waitPhaseAsk(t, h)
			if err := h.Answer(answer); err != nil {
				t.Fatalf("answer: %v", err)
			}
			if err := h.Wait(); err != nil {
				t.Fatalf("run: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !sawAnswer {
				t.Fatal("the model never received the user's answer before later reads")
			}
			if !strings.Contains(laterJoin, answer) {
				t.Fatalf("compaction lost the user's API/dependency answer after later reads; window=%s", clip(laterJoin, 2000))
			}
		})
	}
}

// TestDirect_SkippedMutationNotRememberedAsDone is C2: a real same-turn
// failed check skips a write and a delete. After later reads force
// compaction, working memory must not claim those skipped mutations happened.
func TestDirect_SkippedMutationNotRememberedAsDone(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "keep.txt"), []byte("keep this existing file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("notes%d.txt", i)), []byte(strings.Repeat("ordinary project notes\n", 600)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := directCfg(t, work, data, "Inspect the project and report observations; do not claim skipped changes occurred.")
	cfg.NativeTools = true
	cfg.DirectContextChars = 4000
	step := 0
	var memory string
	var sawSkipped bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		assertDirectToolPairs(t, req.Messages)
		step++
		switch step {
		case 1:
			return nativeResp(
				nativeTC("check", tools.ToolRunBash, map[string]any{"command": "exit 1"}),
				nativeTC("write", tools.ToolWriteFile, map[string]any{"path": "not-created.txt", "content": "new notes"}),
				nativeTC("delete", tools.ToolDeletePath, map[string]any{"path": "keep.txt"}),
			), nil
		case 2:
			for _, m := range req.Messages {
				if m.ToolCallID == "write" && strings.HasPrefix(m.Content, "Not executed:") {
					sawSkipped = true
				}
			}
			return nativeResp(nativeTC("notes0", tools.ToolReadFile, map[string]any{"path": "notes0.txt"})), nil
		case 3:
			return nativeResp(nativeTC("notes1", tools.ToolReadFile, map[string]any{"path": "notes1.txt"})), nil
		default:
			memory = findWorkMemory(req.Messages)
			return nativeResp(nativeTC("done", tools.ToolFinish, map[string]any{"summary": "inspection completed; skipped mutations did not run"})), nil
		}
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawSkipped {
		t.Fatal("did not exercise real failure -> skipped remaining native calls")
	}
	if _, err := os.Stat(filepath.Join(work, "not-created.txt")); !os.IsNotExist(err) {
		t.Fatalf("write was not skipped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "keep.txt")); err != nil {
		t.Fatalf("delete was not skipped: %v", err)
	}
	if memory == "" {
		t.Fatal("fixture did not generate working memory")
	}
	for _, line := range strings.Split(memory, "\n") {
		if strings.HasPrefix(line, "Edited") && strings.Contains(line, "not-created.txt") {
			t.Errorf("working memory claims the skipped write was an edit: %s", line)
		}
		if strings.HasPrefix(line, "Deleted") && strings.Contains(line, "keep.txt") {
			t.Errorf("working memory claims the skipped delete happened: %s", line)
		}
	}
}

// TestDirect_FollowUpWindowStaysBounded is the soft-budget growth gap: pinning
// every historical follow-up lets ordinary short later requests grow without
// bound. The latest request and the original goal must remain; earlier
// instructions, if compressed, must keep labeled historical semantics.
func TestDirect_FollowUpWindowStaysBounded(t *testing.T) {
	makeHistory := func(n int) []llm.Message {
		m := []llm.Message{
			{Role: llm.RoleSystem, Content: "Keep project rules."},
			{Role: llm.RoleUser, Content: "The user's request for this turn:\nMaintain the command-line tool."},
		}
		for i := 0; i < n; i++ {
			m = append(m, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("The earlier tool transcript is still in this conversation. New user request:\nFollow-up %d: preserve existing CLI behavior, extend tests for the current change, and document any remaining work before completing this turn.\n", i)})
			m = append(m, nativeToolPair(fmt.Sprintf("r%d", i), tools.ToolReadFile, map[string]any{"path": fmt.Sprintf("file%d.go", i)}, strings.Repeat("package example\n", 80))...)
		}
		return m
	}
	for _, n := range []int{8, 80} {
		t.Run(fmt.Sprintf("followups_%d", n), func(t *testing.T) {
			m := makeHistory(n)
			w, dropped := directRequestWindow(m, 4000)
			assertDirectToolPairs(t, w)
			joined := joinReq(llm.Request{Messages: w})
			if !strings.Contains(joined, "Keep project rules.") {
				t.Error("window lost the system prompt")
			}
			if !strings.Contains(joined, "Maintain the command-line tool.") {
				t.Error("window lost the original user request")
			}
			latest := fmt.Sprintf("Follow-up %d:", n-1)
			if !strings.Contains(joined, latest) {
				t.Errorf("window lost the latest user request %q", latest)
			}
			got := windowChars(w)
			if got > 8000 {
				t.Errorf("ordinary short follow-ups accumulate outside the 4000-char budget: %d chars (generous 2x bound)", got)
			}
			if n == 80 {
				if dropped == 0 {
					t.Fatal("expected early tool turns to be dropped")
				}
				mem := findWorkMemory(w)
				if mem == "" {
					t.Fatal("expected working memory for the omitted history")
				}
				if !strings.Contains(strings.ToLower(mem), "user instruction") && !strings.Contains(strings.ToLower(mem), "earlier user") && !strings.Contains(mem, "Follow-up") {
					t.Errorf("compressed earlier follow-ups left no historical instruction semantics:\n%s", mem)
				}
			}
		})
	}
}

// TestDirect_SkippedAskCannotSupersedeLatestHumanAnswer is R1: after one
// genuine RunManager.Answer, a later native batch fails a read and skips
// an ask. That Not-executed ask must not take the "latest instruction"
// slot and truncate the still-latest human constraint.
func TestDirect_SkippedAskCannotSupersedeLatestHumanAnswer(t *testing.T) {
	const critical = "Never rename any existing public subcommands."
	const answer = "Keep backward compatibility for the existing command-line interface. Extend the regression coverage around option parsing and error reporting. Prefer a small internal helper rather than adding a dependency, and document the remaining limitations in the final result. Never rename any existing public subcommands."
	work, data := t.TempDir(), t.TempDir()
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("readme%d.txt", i)), []byte(strings.Repeat("ordinary implementation notes\n", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := directCfg(t, work, data, "Clarify the CLI constraints, inspect project notes, then report.")
	cfg.NativeTools = true
	cfg.DirectContextChars = 4000
	cfg.AskTimeout = 15 * time.Second
	var mu sync.Mutex
	step := 0
	var finalWindow string
	var skipped bool
	var asks int
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		assertDirectToolPairs(t, req.Messages)
		mu.Lock()
		defer mu.Unlock()
		step++
		switch step {
		case 1:
			asks++
			return nativeResp(nativeTC("real-ask", "ask", map[string]any{"question": "What compatibility constraints should be kept?"})), nil
		case 2:
			return nativeResp(
				nativeTC("missing-read", tools.ToolReadFile, map[string]any{"path": "not-present.txt"}),
				nativeTC("skipped-ask", "ask", map[string]any{"question": "Which output style?"}),
			), nil
		case 3:
			for _, m := range req.Messages {
				if m.ToolCallID == "skipped-ask" && strings.HasPrefix(m.Content, "Not executed:") {
					skipped = true
				}
			}
			return nativeResp(nativeTC("read0", tools.ToolReadFile, map[string]any{"path": "readme0.txt"})), nil
		case 4:
			return nativeResp(nativeTC("read1", tools.ToolReadFile, map[string]any{"path": "readme1.txt"})), nil
		default:
			finalWindow = joinReq(req)
			return nativeResp(nativeTC("finish", tools.ToolFinish, map[string]any{"summary": "inspection complete"})), nil
		}
	}}
	mgr := NewRunManager()
	t.Cleanup(func() { mgr.Shutdown(2 * time.Second) })
	h, err := mgr.Start(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhaseAsk(t, h)
	if err := h.Answer(answer); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !skipped || asks != 1 {
		t.Fatalf("did not exercise one genuine answer then skipped ask, skipped=%v asks=%d", skipped, asks)
	}
	if !strings.Contains(finalWindow, critical) {
		t.Fatalf("a skipped ask superseded the latest genuine user answer, losing its public-subcommand constraint; window=%s", clip(finalWindow, 2500))
	}
}

func TestDirect_AskResultClassification(t *testing.T) {
	realNative := llm.Message{Role: llm.RoleTool, ToolCallID: "a1", Name: "ask", Content: "User answer:\nKeep the public API."}
	realJSON := llm.Message{Role: llm.RoleUser, Content: "User answer:\nKeep the public API."}
	if !isDirectUserInstruction(realNative) || !isDirectUserInstruction(realJSON) {
		t.Fatal("genuine ask answers must count as user instructions")
	}
	for _, empty := range []llm.Message{
		{Role: llm.RoleUser, Content: "User answer:\n"},
		{Role: llm.RoleUser, Content: "User answer:\n   \t"},
		{Role: llm.RoleTool, ToolCallID: "empty", Name: "ask", Content: "User answer:\n"},
		{Role: llm.RoleTool, ToolCallID: "blank", Name: "ask", Content: "User answer:\n   \t"},
	} {
		if isDirectUserInstruction(empty) {
			t.Errorf("empty %s %q must not count as a user instruction", empty.Role, empty.Content)
		}
	}
	for _, content := range []string{
		"Not executed: an earlier tool in the same model reply failed or returned a non-zero exit. Inspect that result, repair if needed, and re-issue only what is still appropriate. Do not assume this tool ran.",
		"ERROR: ask failed",
		"ask has external side effects and is blocked in manual approval mode: merging or rejecting files is not authorization for network or browser actions, and ask is not authorization. Grant a distinct action consent or disable this tool combination",
		"action consent was not granted",
		"denied: user canceled the question",
		"",
	} {
		native := llm.Message{Role: llm.RoleTool, ToolCallID: "skip", Name: "ask", Content: content}
		if isDirectUserInstruction(native) {
			t.Errorf("native ask result %q must not count as a user instruction", clip(content, 80))
		}
		wrapped := llm.Message{Role: llm.RoleUser, Content: "Tool ask result:\n" + content}
		if isDirectUserInstruction(wrapped) {
			t.Errorf("JSON ask result %q must not count as a user instruction", clip(content, 80))
		}
	}
}
