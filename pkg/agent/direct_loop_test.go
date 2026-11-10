package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
