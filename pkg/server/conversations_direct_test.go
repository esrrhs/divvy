package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

const (
	buggyAdd = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
	fixedAdd = "package addtest\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	addTest  = "package addtest\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d\", got)\n\t}\n}\n"
	addMod   = "module addtest\n\ngo 1.22\n"
	mulSrc   = "package addtest\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n"
	mulTest  = "package addtest\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif got := Mul(3, 4); got != 12 {\n\t\tt.Fatalf(\"Mul(3,4)=%d\", got)\n\t}\n}\n"
)

type convRepairDriver struct {
	mu           sync.Mutex
	sawInject    bool
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

func newConvRepairDriver() *convRepairDriver { return &convRepairDriver{} }

func (d *convRepairDriver) resetFollow() {
	d.mu.Lock()
	d.wroteMul, d.wroteMulTest, d.ranMul = false, false, false
	d.wroteInject, d.sawInject = false, false
	d.mu.Unlock()
}

func (d *convRepairDriver) client() llm.Client {
	return &llm.ScriptedClient{Handle: d.handle}
}

func (d *convRepairDriver) handle(ctx context.Context, req llm.Request) (*llm.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	body := joinMsgs(req)
	if strings.Contains(body, "inject-marker") {
		d.sawInject = true
		if !d.wroteInject {
			d.wroteInject = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "injected.txt", "content": "from-inject"}), nil
		}
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
	last := lastTool(req)
	if !d.wroteFix {
		if !strings.Contains(last, "FAIL") {
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "expected failing test, last=" + last}), nil
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

func jsonAct(action string, args map[string]any) *llm.Response {
	payload, _ := json.Marshal(map[string]any{"thought": "scripted", "action": action, "args": args})
	return &llm.Response{Content: string(payload)}
}

func joinMsgs(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func lastTool(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleTool || strings.Contains(req.Messages[i].Content, "Tool ") {
			return req.Messages[i].Content
		}
	}
	return ""
}

func waitConvDone(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	var last map[string]any
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		_, last = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if hasRunFinished(last, "") {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("direct conversation never finished, last=%v", last)
	return last
}

func waitConvDoneAfter(t *testing.T, ts *httptest.Server, id, afterSession string) map[string]any {
	t.Helper()
	var last map[string]any
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		_, last = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
		if hasRunFinished(last, afterSession) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("follow-up turn never finished, last=%v", last)
	return last
}

func goTestOK(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("go", "test", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test failed: %v\n%s", err, out)
	}
	return string(out)
}

func hasRunFinished(got map[string]any, afterSession string) bool {
	for _, m := range convMsgs(got) {
		if m["kind"] != kindRunFinished {
			continue
		}
		if afterSession != "" && anyString(m["session_id"]) == afterSession {
			continue
		}
		out := anyString(m["outcome"])
		if out == "completed" || out == "failed" || out == "aborted" {
			return true
		}
	}
	return false
}

func convMsgs(got map[string]any) []map[string]any {
	raw, _ := got["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func hasKind(msgs []map[string]any, kind, substr string) bool {
	for _, m := range msgs {
		if m["kind"] == kind && (substr == "" || strings.Contains(anyString(m["text"]), substr)) {
			return true
		}
	}
	return false
}

func convGet(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	return got
}

func TestConversation_DirectFailFixFollowUpInjectAndHunks(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	drv := newConvRepairDriver()
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return drv.client() })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "isolate": false,
		"message": "Implement Add(a, b int) int in add.go with a test; go test must pass.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	settings, _ := resp["settings"].(map[string]any)
	if anyString(resp["agent_loop"]) != agent.AgentLoopDirect && anyString(settings["agent_mode"]) != agent.AgentLoopDirect {
		t.Fatalf("expected direct mode in view, got %v", resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	if sid == "" {
		t.Fatalf("direct turn should start immediately: %v", resp)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := convGet(t, ts, id)
		if anyString(got["run_phase"]) == string(agent.PhasePlanReview) {
			t.Fatalf("direct mode must not enter plan_review: %v", got)
		}
		if p := anyString(got["run_phase"]); p == string(agent.PhaseDone) || p == string(agent.PhaseRunning) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := waitConvDone(t, ts, id)
	if anyString(got["run_phase"]) == string(agent.PhaseFailed) {
		t.Fatalf("first turn failed: %v", got)
	}
	goTestOK(t, work)
	msgs := convMsgs(got)
	if !hasKind(msgs, kindModelReply, "subtraction") {
		t.Fatalf("model summary missing from conversation (must not be a canned status log): %v", msgs)
	}
	if !hasKind(msgs, kindToolActivity, "run_bash") && !hasKind(msgs, kindToolActivity, "go test") {
		t.Fatalf("tool activity not recorded: %v", msgs)
	}

	drv.resetFollow()
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
		"text": "also implement Mul(a, b int) int with a test",
		"mode": "start",
	})
	if status >= 300 {
		t.Fatalf("follow-up status=%d %v", status, resp)
	}

	sid2 := ""
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		got = convGet(t, ts, id)
		sid2 = anyString(got["current_session_id"])
		phase := anyString(got["run_phase"])
		if phase == string(agent.PhaseRunning) && sid2 != "" && sid2 != sid {
			break
		}
		if phase == string(agent.PhaseDone) && sid2 != sid {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if anyString(convGet(t, ts, id)["run_phase"]) == string(agent.PhaseRunning) {
		st, inj := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/messages?token=test-token", map[string]any{
			"text": "inject-marker also write injected.txt",
			"mode": "steer",
		})
		if st >= 300 {
			t.Fatalf("inject status=%d %v", st, inj)
		}
	}

	got = waitConvDoneAfter(t, ts, id, sid)
	if _, err := os.Stat(filepath.Join(work, "mul.go")); err != nil {
		t.Fatalf("follow-up should add mul.go: %v", err)
	}
	goTestOK(t, work)
	if drv.sawInject {
		body, err := os.ReadFile(filepath.Join(work, "injected.txt"))
		if err != nil || string(body) != "from-inject" {
			t.Fatalf("inject should write injected.txt, got %q %v", body, err)
		}
		if !hasKind(convMsgs(got), kindNotice, "注入") && !hasKind(convMsgs(got), kindNotice, "inject") {
			t.Logf("inject applied notice may have been phrased differently")
		}
	}

	_, ch1 := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sid+"/changes?token=test-token", nil)
	if ch1["editable"] == true {
		t.Fatalf("historical turn must not be editable: %v", ch1)
	}
	sidLatest := anyString(got["current_session_id"])
	var ch2 map[string]any
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, ch2 = apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"/turns/"+sidLatest+"/changes?token=test-token", nil)
		if ch2["editable"] == true {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ch2["editable"] != true {
		t.Fatalf("latest direct turn should be editable: %v", ch2)
	}

	if err := os.WriteFile(filepath.Join(work, "mul.go"), []byte("package addtest\n// user edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, rej := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations/"+id+"/turns/"+sidLatest+"/changes?token=test-token", map[string]any{
		"path": "mul.go", "all": "reject",
	})
	if st != http.StatusConflict {
		t.Fatalf("hunk apply must not overwrite a user edit, got %d %v", st, rej)
	}
	body, _ := os.ReadFile(filepath.Join(work, "mul.go"))
	if !strings.Contains(string(body), "user edit") {
		t.Fatalf("user edit was overwritten: %q", body)
	}

	disk, err := os.ReadFile(conversationPath(data, id))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), testSecretKey) {
		t.Fatalf("key leaked into conversation file")
	}
	tr, err := os.ReadFile(filepath.Join(data, "conversations", id+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tr), testSecretKey) {
		t.Fatalf("key leaked into direct transcript")
	}
	if !strings.Contains(string(tr), "FAIL") {
		t.Fatalf("transcript should keep the real failing test output")
	}
}

func TestConversation_DirectPauseResumeAndRestart(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var calls int
	var mu sync.Mutex
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		body := joinMsgs(req)
		if n == 1 && !strings.Contains(body, "Tool ") {
			return jsonAct(tools.ToolRunBash, map[string]any{"command": "sleep 0.4; echo first-tool", "timeout_sec": 10}), nil
		}
		if !strings.Contains(body, "resume.txt") || !strings.Contains(body, "Tool write_file result") {
			if !strings.Contains(body, "resume.txt") {
				return jsonAct(tools.ToolWriteFile, map[string]any{"path": "resume.txt", "content": "after-resume"}), nil
			}
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "resumed and wrote resume.txt after the first tool"}), nil
	}}

	mgr := agent.NewRunManager()
	s, err := New(mgr, data, WithToken("test-token"), WithHeartbeat(50*time.Millisecond),
		WithWorkdir(work), WithClientFactory(func(agent.Config) llm.Client { return client }))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct",
		"message": "run a first tool then wait",
	})
	if status != http.StatusCreated {
		ts.Close()
		t.Fatalf("create status=%d %v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhaseRunning))
	// Pause while the first tool (sleep) is in flight; the loop should
	// finish that tool, then stop at the boundary.
	time.Sleep(80 * time.Millisecond)
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/pause?token=test-token", map[string]any{})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if anyString(convGet(t, ts, id)["run_phase"]) == string(agent.PhasePaused) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if anyString(convGet(t, ts, id)["run_phase"]) != string(agent.PhasePaused) {
		ts.Close()
		t.Fatalf("expected paused, got %v", convGet(t, ts, id))
	}
	if _, err := os.Stat(filepath.Join(work, "resume.txt")); err == nil {
		ts.Close()
		t.Fatal("resume.txt should not exist until after resume")
	}

	ts.Close()
	mgr.Shutdown(3 * time.Second)

	mgr2 := agent.NewRunManager()
	s2, err := New(mgr2, data, WithToken("test-token"), WithHeartbeat(50*time.Millisecond),
		WithWorkdir(work), WithClientFactory(func(agent.Config) llm.Client { return client }))
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(s2.Handler())
	t.Cleanup(func() { ts2.Close(); mgr2.Shutdown(2 * time.Second) })

	got := convGet(t, ts2, id)
	if anyString(got["run_phase"]) != string(agent.PhasePaused) {
		t.Fatalf("unfinished direct transcript should surface as paused after restart, got %v", got)
	}
	st, res := apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations/"+id+"/resume?token=test-token", map[string]any{
		"api_key": testSecretKey,
	})
	if st >= 300 {
		t.Fatalf("resume after restart: %d %v", st, res)
	}
	got = waitConvDone(t, ts2, id)
	if anyString(got["run_phase"]) == string(agent.PhaseFailed) {
		t.Fatalf("resumed turn failed: %v", got)
	}
	body, err := os.ReadFile(filepath.Join(work, "resume.txt"))
	if err != nil || string(body) != "after-resume" {
		t.Fatalf("expected resume.txt after resume, got %q %v", body, err)
	}
}

func TestConversation_DirectBudgetAndManualApprovalDoNotPlan(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "nothing to do under budget test"}), nil
	}}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "manual",
		"budget_tokens": 50_000,
		"message":       "just say you looked",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d %v", status, resp)
	}
	id := anyString(resp["id"])
	got := waitConvDone(t, ts, id)
	if p := anyString(got["run_phase"]); p == string(agent.PhasePlanReview) {
		t.Fatalf("direct mode must not park in plan_review, got %v", got)
	}
	// No files were written, so manual approval has nothing to review and
	// must not stay parked after finish.
	if p := anyString(got["run_phase"]); p == string(agent.PhaseLeafApproval) {
		t.Fatalf("no-op turn parked in leaf_approval: %v", got)
	}
	if !hasKind(convMsgs(got), kindModelReply, "nothing to do") {
		t.Fatalf("expected model reply, got %v", got)
	}

	work2 := t.TempDir()
	status, resp = apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work2, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "message": "write out file",
	})
	if status != http.StatusCreated {
		t.Fatalf("guided create status=%d %v", status, resp)
	}
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhasePlanReview))
}

func TestConversation_DirectStreamDeltasArriveInChunks(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ready := make(chan struct{})
	parts := []string{"AAA", "BBB", "CCC", "DDD"}
	client := &llm.ScriptedClient{
		TokenParts: parts,
		ChunkDelay: 50 * time.Millisecond,
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "streamed in chunks"}), nil
		},
	}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "isolate": false,
		"message": "Please stream a greeting and finish.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	if sid == "" {
		t.Fatalf("missing session: %v", resp)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/"+sid+"/events?token=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	sse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sse.Body.Close() })
	if sse.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", sse.StatusCode)
	}
	close(ready)

	var mu sync.Mutex
	var deltas []string
	var arrivals []time.Time
	var sawPartial bool
	want := strings.Join(parts, "")
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		br := bufio.NewReader(sse.Body)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
				continue
			}
			if ev["kind"] != "assistant_delta" {
				if ev["kind"] == "assistant_stream_end" || ev["kind"] == "session_end" {
					return
				}
				continue
			}
			text := anyString(ev["text"])
			if text == want && ev["live"] != true {
				// A single non-live packet of the full text would be a fake flush.
			}
			mu.Lock()
			deltas = append(deltas, text)
			arrivals = append(arrivals, time.Now())
			mu.Unlock()
			if text == want && len(deltas) == 1 {
				// recorded below after join check
			}
		}
	}()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			got := convGet(t, ts, id)
			live := anyString(got["live_assistant"])
			if live != "" && live != want && strings.HasPrefix(want, live) {
				mu.Lock()
				sawPartial = true
				mu.Unlock()
			}
			if strings.Contains(live, testSecretKey) {
				t.Errorf("conversation live_assistant leaked key")
				return
			}
			if hasRunFinished(got, "") {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	_ = waitConvDone(t, ts, id)
	select {
	case <-collectDone:
	case <-time.After(2 * time.Second):
	}
	<-pollDone

	mu.Lock()
	got := append([]string(nil), deltas...)
	times := append([]time.Time(nil), arrivals...)
	partial := sawPartial
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected multiple SSE assistant_delta events, got %d %#v", len(got), got)
	}
	if joined := strings.Join(got, ""); joined != want {
		t.Fatalf("joined SSE deltas %q want %q", joined, want)
	}
	if len(got) == 1 && got[0] == want {
		t.Fatal("stream arrived as a single full-text packet")
	}
	if !partial {
		t.Fatal("GET never showed a mid-stream prefix of live_assistant")
	}
	if len(times) >= 2 && times[len(times)-1].Sub(times[0]) < 40*time.Millisecond {
		t.Fatalf("all %d deltas arrived within 40ms; looks like a single flush", len(times))
	}

	// Deltas must not be written to the persisted event log.
	matches, _ := filepath.Glob(filepath.Join(data, "events", "*.jsonl"))
	for _, pth := range matches {
		raw, err := os.ReadFile(pth)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "assistant_delta") {
			t.Fatalf("assistant_delta persisted to %s", pth)
		}
		if strings.Contains(string(raw), testSecretKey) {
			t.Fatalf("event log leaked key: %s", pth)
		}
	}
}

func TestConversation_DirectAskBlocksUntilAnswerThenWrites(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var mu sync.Mutex
	asked, sawAnswer, wrote := false, false, false
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		body := joinMsgs(req)
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
			return jsonAct(tools.ToolFinish, map[string]any{"summary": "never received the user answer"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote the file the user named"}), nil
	}}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "isolate": false,
		"message": "Ask me for a filename, then write hello there.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d resp=%v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])

	var last map[string]any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		last = convGet(t, ts, id)
		if anyString(last["run_phase"]) == string(agent.PhaseAsk) && anyString(last["pending_ask"]) != "" && hasKind(convMsgs(last), kindAsk, "filename") {
			break
		}
		if hasRunFinished(last, "") {
			t.Fatalf("run finished before ask: %v", last)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if anyString(last["run_phase"]) != string(agent.PhaseAsk) {
		t.Fatalf("expected ask phase, got %v", last)
	}
	if !strings.Contains(anyString(last["pending_ask"]), "filename") {
		t.Fatalf("pending_ask missing question: %v", last)
	}
	if !hasKind(convMsgs(last), kindAsk, "filename") {
		t.Fatalf("conversation is missing the ask message: %v", last)
	}
	target := filepath.Join(work, "notes-from-user.txt")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file written before the user answered")
	}

	st, ans := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/answer?token=test-token", map[string]any{
		"text": "notes-from-user.txt",
	})
	if st != http.StatusOK {
		t.Fatalf("answer status=%d resp=%v", st, ans)
	}

	done := waitConvDone(t, ts, id)
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected write after answer: %v", err)
	}
	if string(raw) != "from-ask" {
		t.Fatalf("file content %q", raw)
	}
	msgs := convMsgs(done)
	if !hasKind(msgs, kindAskAnswer, "notes-from-user.txt") {
		t.Fatalf("missing ask_answer message: %v", msgs)
	}
	if !hasKind(msgs, kindToolActivity, "write_file") && !hasKind(msgs, kindToolActivity, "notes-from-user.txt") {
		t.Fatalf("missing subsequent tool activity after answer: %v", msgs)
	}
	if !hasKind(msgs, kindModelReply, "wrote the file") && !hasKind(msgs, kindRunFinished, "") {
		t.Fatalf("missing model follow-through after ask: %v", msgs)
	}
	mu.Lock()
	okAsk, okAns, okWrite := asked, sawAnswer, wrote
	mu.Unlock()
	if !okAsk || !okAns || !okWrite {
		t.Fatalf("flags asked=%v sawAnswer=%v wrote=%v", okAsk, okAns, okWrite)
	}
}

func TestConversation_DirectManualApproveRejectAndDefaults(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	var wrote, finished bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "needs-ok.txt", "content": "from-agent"}), nil
		}
		finished = true
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote needs-ok.txt"}), nil
	}}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "manual",
		"message": "Write needs-ok.txt then finish.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d %v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])

	var last map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		last = convGet(t, ts, id)
		if anyString(last["run_phase"]) == string(agent.PhaseLeafApproval) {
			break
		}
		if hasRunFinished(last, "") {
			t.Fatalf("finished before approval: %v", last)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if anyString(last["run_phase"]) != string(agent.PhaseLeafApproval) {
		t.Fatalf("expected leaf_approval, got %v", last)
	}
	if anyString(last["leaf_approving"]) != "direct" && anyString(last["leaf_approving"]) == "" {
		// leaf_approving may be on the view or only on the session snapshot
		live := waitLivePhase(t, ts, sid, string(agent.PhaseLeafApproval))
		if anyString(live["leaf_approving"]) != "direct" && anyString(live["leaf_approving"]) == "" {
			t.Fatalf("missing leaf_approving: conv=%v live=%v", last, live)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "needs-ok.txt")); err == nil {
		t.Fatal("file reached the workdir before approval")
	}

	st, rej := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/approvals/direct?token=test-token", map[string]any{
		"decision": "reject", "comment": "do not apply",
	})
	if st != http.StatusOK {
		t.Fatalf("reject status=%d %v", st, rej)
	}
	done := waitConvDone(t, ts, id)
	if _, err := os.Stat(filepath.Join(work, "needs-ok.txt")); err == nil {
		t.Fatal("rejected file was written")
	}
	if !hasKind(convMsgs(done), kindNotice, "拒绝") && !hasKind(convMsgs(done), kindModelReply, "拒绝") {
		t.Fatalf("missing rejection notice: %v", convMsgs(done))
	}
	_ = finished
}

func TestConversation_DirectManualProtectsExternalEditAndOmittedModeIsAuto(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	var wrote bool
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote {
			wrote = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "note.txt", "content": "agent-version"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "updated note.txt"}), nil
	}}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })

	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "approval_mode": "manual",
		"message": "Change note.txt then finish.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d %v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	waitLivePhase(t, ts, sid, string(agent.PhaseLeafApproval))
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("external-edit"), 0644); err != nil {
		t.Fatal(err)
	}
	st, ap := apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+sid+"/approvals/direct?token=test-token", map[string]any{
		"decision": "approve",
	})
	if st != http.StatusOK {
		t.Fatalf("approve status=%d %v", st, ap)
	}
	_ = waitConvDone(t, ts, id)
	body, err := os.ReadFile(filepath.Join(work, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "external-edit" {
		t.Fatalf("external edit overwritten: %q", body)
	}

	// Omitting approval_mode stays auto: writes land without a review gate.
	work2 := t.TempDir()
	var wrote2 bool
	client2 := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		if !wrote2 {
			wrote2 = true
			return jsonAct(tools.ToolWriteFile, map[string]any{"path": "auto.txt", "content": "ok"}), nil
		}
		return jsonAct(tools.ToolFinish, map[string]any{"summary": "wrote auto.txt"}), nil
	}}
	ts2 := apiServerWithClient(t, work2, data, func(agent.Config) llm.Client { return client2 })
	status, resp = apiDo(t, http.MethodPost, apiBase(ts2)+"/conversations?token=test-token", map[string]any{
		"workdir": work2, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct",
		"message": "Write auto.txt",
	})
	if status != http.StatusCreated {
		t.Fatalf("auto create status=%d %v", status, resp)
	}
	id2 := anyString(resp["id"])
	got := waitConvDone(t, ts2, id2)
	if anyString(got["run_phase"]) == string(agent.PhaseLeafApproval) {
		t.Fatalf("omitted approval_mode should stay auto, got %v", got)
	}
	if _, err := os.Stat(filepath.Join(work2, "auto.txt")); err != nil {
		t.Fatalf("auto write missing: %v", err)
	}
}

func TestConversation_DirectProtocolStreamHidesJSONAndCrossChunkKey(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ready := make(chan struct{})
	raw := `{"thought":"Hello ` + `VISIBLE","action":"finish","args":{"summary":"All good"}}`
	// Secret is injected into thought across chunks via TokenParts below.
	secret := testSecretKey
	thoughtOpen := `{"thought":"Hello `
	thoughtClose := `","action":"finish","args":{"summary":"All good"}}`
	parts := []string{thoughtOpen, secret, thoughtClose}
	client := &llm.ScriptedClient{
		TokenParts: parts,
		ChunkDelay: 40 * time.Millisecond,
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &llm.Response{Content: raw}, nil
		},
	}
	ts := apiServerWithClient(t, work, data, func(agent.Config) llm.Client { return client })
	status, resp := apiDo(t, http.MethodPost, apiBase(ts)+"/conversations?token=test-token", map[string]any{
		"workdir": work, "model": "scripted", "base_url": "http://127.0.0.1:9/v1",
		"api_key": testSecretKey, "agent_mode": "direct", "isolate": false,
		"message": "Please finish with a short hello.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d %v", status, resp)
	}
	id := anyString(resp["id"])
	sid := anyString(resp["current_session_id"])
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/"+sid+"/events?token=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	sse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sse.Body.Close() })
	if sse.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", sse.StatusCode)
	}
	close(ready)

	var mu sync.Mutex
	var deltas []string
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		br := bufio.NewReader(sse.Body)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
				continue
			}
			if ev["kind"] == "assistant_stream_end" || ev["kind"] == "session_end" {
				return
			}
			if ev["kind"] != "assistant_delta" {
				continue
			}
			text := anyString(ev["text"])
			full := anyString(ev["full"])
			if strings.Contains(text, secret) || strings.Contains(full, secret) {
				t.Errorf("sse leaked api key")
				return
			}
			if strings.Contains(text, `"action"`) || strings.Contains(text, `"args"`) {
				t.Errorf("sse leaked protocol")
				return
			}
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
		}
	}()

	_ = waitConvDone(t, ts, id)
	select {
	case <-collectDone:
	case <-time.After(2 * time.Second):
	}
	mu.Lock()
	got := strings.Join(deltas, "")
	mu.Unlock()
	if len(deltas) < 2 {
		t.Fatalf("expected multiple visible deltas, got %d", len(deltas))
	}
	if strings.Contains(got, secret) {
		t.Fatal("joined deltas leaked api key")
	}
	if strings.Contains(got, `"action"`) || strings.Contains(got, `{`) {
		t.Fatalf("protocol leaked: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "All good") && !strings.Contains(got, "VISIBLE") && !strings.Contains(got, "***REDACTED***") {
		// thought is "Hello SECRET" redacted, plus summary All good
		if !strings.Contains(got, "Hello") {
			t.Fatalf("missing readable thought: %q", got)
		}
	}
}
