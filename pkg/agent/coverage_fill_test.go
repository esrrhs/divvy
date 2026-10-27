package agent

// Tests in this file fill coverage gaps on error branches and edge cases that
// the scenario-driven suites do not reach: id/backoff helpers, worker tool
// plumbing, guided-mode control branches, decomposer validation, event
// recorder boundaries, REPL commands, report roll-ups and acceptance routing.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// --- ids.go ---------------------------------------------------------------

func TestSanitizeIDEdges(t *testing.T) {
	cases := map[string]string{
		"Hello World":    "hello_world",
		"a_b-c":          "a_b_c", // separators collapse to one underscore
		"a  b/c.d":       "a_b_c_d",
		"!!!":            "task", // nothing usable
		"-_-":            "task", // only separators
		"_leading":       "leading",
		"trailing-":      "trailing",
		"日本語 go":         "go", // non-ASCII dropped
		"UPPER_with_123": "upper_with_123",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
	// Long ids are cut to 40 runes and re-trimmed of edge separators.
	if got := sanitizeID(strings.Repeat("a", 45)); got != strings.Repeat("a", 40) {
		t.Errorf("sanitizeID(long) = %q", got)
	}
	// 41 chars: the cut keeps a trailing separator, which is trimmed away.
	if got := sanitizeID(strings.Repeat("a", 39) + "-b"); got != strings.Repeat("a", 39) {
		t.Errorf("sanitizeID(cut+trim) = %q", got)
	}
}

func TestUniqueIDFallbacks(t *testing.T) {
	// Empty and reserved bases become "task".
	if got := uniqueID("", func(string) bool { return false }); got != "task" {
		t.Errorf("uniqueID(\"\") = %q", got)
	}
	if got := uniqueID("root", func(string) bool { return false }); got != "task" {
		t.Errorf("uniqueID(root) = %q", got)
	}
	// Collisions walk the _2, _3, ... sequence.
	taken := func(cand string) bool { return cand == "task" || cand == "task_2" }
	if got := uniqueID("task", taken); got != "task_3" {
		t.Errorf("uniqueID(collision) = %q", got)
	}
	// Everything up to 999 taken → the _1000 escape hatch.
	all := func(string) bool { return true }
	if got := uniqueID("x", all); got != "x_1000" {
		t.Errorf("uniqueID(exhausted) = %q", got)
	}
}

func TestLooksLikePathShapes(t *testing.T) {
	yes := []string{"a/b.txt", "file.go", "main.go", "./x"}
	no := []string{"", "   ", "plain", "two words", "tab\there", "new\nline"}
	for _, s := range yes {
		if !looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = false", s)
		}
	}
	for _, s := range no {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true", s)
		}
	}
}

// --- backoff.go -----------------------------------------------------------

func TestRetryDelayBoundaries(t *testing.T) {
	// Zero/negative knobs fall back to the defaults.
	if d := RetryDelay(0, 0, 0); d != time.Second {
		t.Errorf("RetryDelay(0,0,0) = %s", d)
	}
	// max < min clamps max to min.
	if d := RetryDelay(1, 5*time.Second, 2*time.Second); d != 5*time.Second {
		t.Errorf("RetryDelay max<min = %s", d)
	}
	// Doubling stops as soon as half the max is crossed (early return).
	if d := RetryDelay(4, time.Second, 6*time.Second); d != 6*time.Second {
		t.Errorf("RetryDelay early cap = %s", d)
	}
}

func TestWaitBackoffZeroDelay(t *testing.T) {
	if err := waitBackoff(context.Background(), 0); err != nil {
		t.Errorf("zero delay with live context = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBackoff(ctx, -time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("zero delay with canceled context = %v", err)
	}
}

func TestConfigRetryIntervals(t *testing.T) {
	var zero Config
	if zero.retryMin() != defaultRetryMinInterval || zero.retryMax() != defaultRetryMaxInterval {
		t.Fatalf("defaults: %s %s", zero.retryMin(), zero.retryMax())
	}
	c := Config{RetryMinInterval: 3 * time.Second, RetryMaxInterval: 9 * time.Second}
	if c.retryMin() != 3*time.Second || c.retryMax() != 9*time.Second {
		t.Fatalf("explicit values lost: %s %s", c.retryMin(), c.retryMax())
	}
}

// --- worker.go pure helpers ------------------------------------------------

func TestCollectActionsToolCalls(t *testing.T) {
	resp := &llm.Response{ToolCalls: []llm.ToolCall{
		{ID: "c1", Function: llm.ToolFunction{Name: "Write_File", Arguments: `{"path":"a.txt"}`}},
		{ID: "c2", Function: llm.ToolFunction{Name: "finish", Arguments: `not json`}},
	}}
	acts := collectActions(resp)
	if len(acts) != 2 {
		t.Fatalf("got %d actions", len(acts))
	}
	if acts[0].ID != "c1" || acts[0].Name != "write_file" || acts[0].Args["path"] != "a.txt" {
		t.Fatalf("action 0 = %+v", acts[0])
	}
	// Unparseable args leave an empty (non-nil) map.
	if acts[1].Args == nil || len(acts[1].Args) != 0 {
		t.Fatalf("bad args must degrade to an empty map, got %+v", acts[1].Args)
	}
}

func TestCollectActionFallbacks(t *testing.T) {
	// Empty content: nothing to act on.
	if acts := collectActions(&llm.Response{Content: "   "}); acts != nil {
		t.Fatalf("empty content must give no actions, got %+v", acts)
	}
	// Content without any JSON: unparseable, no actions.
	if acts := collectActions(&llm.Response{Content: "I cannot help with that."}); acts != nil {
		t.Fatalf("prose must give no actions, got %+v", acts)
	}
}

func TestPreviewArgsAllKinds(t *testing.T) {
	cases := []taggedAction{
		{Name: tools.ToolReadFile, Args: map[string]any{"path": "a.go"}},
		{Name: tools.ToolWriteFile, Args: map[string]any{"path": "b.go"}},
		{Name: tools.ToolReplaceLines, Args: map[string]any{"path": "c.go"}},
		{Name: tools.ToolListDir, Args: map[string]any{"path": "dir"}},
		{Name: tools.ToolDeletePath, Args: map[string]any{"path": "old"}},
		{Name: tools.ToolOutline, Args: map[string]any{"path": "big.go"}},
		{Name: tools.ToolMovePath, Args: map[string]any{"from": "x", "to": "y"}},
		{Name: tools.ToolDownloadFile, Args: map[string]any{"url": "http://u", "path": "f"}},
		{Name: tools.ToolSQLiteQuery, Args: map[string]any{"path": "db.sqlite"}},
		{Name: tools.ToolJSONQuery, Args: map[string]any{"path": "d.json"}},
		{Name: tools.ToolRunBash, Args: map[string]any{"command": "echo hi"}},
		{Name: tools.ToolSearchFiles, Args: map[string]any{"pattern": "p", "glob": "*.go"}},
		{Name: tools.ToolFindFiles, Args: map[string]any{"pattern": "p", "path": "dir"}},
		{Name: tools.ToolFinish, Args: map[string]any{"summary": "done"}},
		{Name: "mystery_tool"},
	}
	for _, act := range cases {
		_ = previewArgs(act) // must not panic; every switch arm returns
	}
	if p := previewArgs(taggedAction{Name: tools.ToolMovePath, Args: map[string]any{"from": "x", "to": "y"}}); !strings.Contains(p, "move x -> y") {
		t.Errorf("move preview = %q", p)
	}
	if p := previewArgs(taggedAction{Name: tools.ToolRunBash, Args: map[string]any{"command": strings.Repeat("z", 100)}}); len(p) > 83 {
		t.Errorf("run_bash preview not truncated: %q", p)
	}
}

func TestStringFromArgsMissing(t *testing.T) {
	if _, ok := stringFromArgs(nil, "k"); ok {
		t.Error("nil args must report missing")
	}
	if _, ok := stringFromArgs(map[string]any{"other": 1}, "k"); ok {
		t.Error("absent key must report missing")
	}
	if _, ok := stringFromArgs(map[string]any{"k": 42}, "k"); ok {
		t.Error("non-string value must report missing")
	}
	if s, ok := stringFromArgs(map[string]any{"k": "v"}, "k"); !ok || s != "v" {
		t.Errorf("string value = %q ok=%v", s, ok)
	}
}

func TestTrimHistoryAndSplitsEdges(t *testing.T) {
	// Fewer messages than the head: returned untouched.
	small := []llm.Message{{Role: llm.RoleSystem, Content: "s"}}
	if got := trimHistory(small, 0); len(got) != 1 {
		t.Fatalf("trimHistory below head = %d messages", len(got))
	}
	// A cut at the very end is never treated as splitting a group.
	if splitsToolGroup(small, len(small)) {
		t.Error("cut past the last message cannot split a group")
	}
}

// --- worker.go runWorker ---------------------------------------------------

// newWorkerOrchestrator builds a minimal orchestrator whose root is a leaf.
func newWorkerOrchestrator(t *testing.T, client llm.Client) (*Orchestrator, *tools.Sandbox) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Goal = "worker gap test"
	cfg.Stream = false
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	sb, err := tools.NewSandbox(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	return o, sb
}

func TestWorkerNativeToolCallsRoundTrip(t *testing.T) {
	// Native (OpenAI-style) tool calls: defs are attached, results go back as
	// role:"tool" messages, and the finish tool call ends the attempt.
	var sawToolsInRequest bool
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		if len(req.Tools) > 0 {
			sawToolsInRequest = true
		}
		switch {
		case !toolResultSeenNative(req, "write_file"):
			return &llm.Response{ToolCalls: []llm.ToolCall{{
				ID:       "call-1",
				Function: llm.ToolFunction{Name: "write_file", Arguments: `{"path":"native.txt","content":"hi"}`},
			}}}, nil
		default:
			return &llm.Response{ToolCalls: []llm.ToolCall{{
				ID:       "call-2",
				Function: llm.ToolFunction{Name: "finish", Arguments: `{"summary":"native done"}`},
			}}}, nil
		}
	}}
	o, sb := newWorkerOrchestrator(t, client)
	o.cfg.NativeTools = true

	summary, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "native done" {
		t.Fatalf("summary = %q", summary)
	}
	if !sawToolsInRequest {
		t.Fatal("native tool defs were not attached to the request")
	}
	// The tool result must have been fed back with the role/tool shape,
	// bound to the originating call id.
	found := false
	for _, req := range client.Requests {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && m.ToolCallID == "call-1" && m.Name == "write_file" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tool result not delivered as a role:tool message")
	}
}

func toolResultSeenNative(req llm.Request, tool string) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool && m.Name == tool {
			return true
		}
	}
	return false
}

func TestWorkerRepromptOnUnparseable(t *testing.T) {
	// Prose instead of JSON: the worker asks the model to retry, then accepts
	// a valid action on the next turn.
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Could not parse a tool call") {
				return jsonAction("finish", map[string]string{"summary": "recovered"}), nil
			}
		}
		return &llm.Response{Content: "Sorry, the file is fine."}, nil
	}}
	o, sb := newWorkerOrchestrator(t, client)
	summary, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "recovered" {
		t.Fatalf("summary = %q", summary)
	}
	if len(client.Requests) < 2 {
		t.Fatalf("expected a reprompt round, got %d calls", len(client.Requests))
	}
}

func TestWorkerToolErrorFeedback(t *testing.T) {
	// A failing tool call feeds "ERROR: ..." back to the model, which then
	// finishes anyway.
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		if toolResultSeen(req, "read_file") {
			return jsonAction("finish", map[string]string{"summary": "gave up reading"}), nil
		}
		return jsonAction("read_file", map[string]string{"path": "no/such/file.txt"}), nil
	}}
	o, sb := newWorkerOrchestrator(t, client)
	if _, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), ""); err != nil {
		t.Fatal(err)
	}
	sawErr := false
	for _, req := range client.Requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "ERROR: ") {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Fatal("tool error was not fed back to the model")
	}
}

func TestWorkerFinishSummaryFallbacks(t *testing.T) {
	// finish without args: summary falls back to thought, then "finished".
	for i, tc := range []struct {
		content string
		want    string
	}{
		{`{"thought":"deep thought","action":"finish"}`, "deep thought"},
		{`{"action":"finish"}`, "finished"},
	} {
		client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: tc.content}, nil
		}}
		o, sb := newWorkerOrchestrator(t, client)
		summary, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
		if err != nil {
			t.Fatal(err)
		}
		if summary != tc.want {
			t.Fatalf("case %d: summary = %q, want %q", i, summary, tc.want)
		}
		o.Close()
	}
}

func TestWorkerMaxStepsFloorAndExhaustion(t *testing.T) {
	// MaxSteps < 1 floors at 12; a model that never calls finish fails the
	// attempt with ErrMaxSteps instead of returning success.
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return jsonAction("list_dir", map[string]string{"path": "."}), nil
	}}
	o, sb := newWorkerOrchestrator(t, client)
	o.cfg.MaxSteps = 0

	_, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("want ErrMaxSteps, got %v", err)
	}
	if got := len(client.Requests); got != 12 {
		t.Fatalf("expected 12 steps after flooring MaxSteps, got %d", got)
	}
}

func TestWorkerFinishGateWarnsButAccepts(t *testing.T) {
	// A git repo whose only finding is a leftover debug print: the gate warns
	// (non-blocking) and finish is accepted.
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, ".gitkeep"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.Goal = "warn gate"
	cfg.Stream = false
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	sb, err := tools.NewSandbox(work)
	if err != nil {
		t.Fatal(err)
	}
	// The untracked file carries a debug statement: warning, not blocking.
	if err := os.WriteFile(filepath.Join(work, "dbg.go"), []byte("package main\n\nfunc main() { fmt.Println(\"debug\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return jsonAction("finish", map[string]string{"summary": "warned but done"}), nil
	}}
	o.llm = client
	summary, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "warned but done" {
		t.Fatalf("non-blocking gate must accept finish, got %q", summary)
	}
	// Sanity: the same gate really was non-blocking for this tree.
	gate, gerr := sb.PreFinishGate(context.Background())
	if gerr != nil || gate == nil || gate.Blocking {
		t.Fatalf("expected a non-blocking gate, got gate=%v err=%v", gate, gerr)
	}
}

func TestWorkerFinishGateBlockedNativeFeedback(t *testing.T) {
	// Native tool calls + a conflict-marked file: finish is blocked and the
	// rejection is delivered as a role:tool message; after the repair the
	// same finish tool call is accepted.
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, ".gitkeep"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")

	const conflictContent = `{"path":"x.txt","content":"<<<<<<< HEAD\nkeep\n=======\n>>>>>>> b\n"}`
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		rejected := false
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "finish REJECTED") {
				rejected = true
			}
		}
		toolcall := func(id, name, args string) *llm.Response {
			return &llm.Response{ToolCalls: []llm.ToolCall{{
				ID:       id,
				Function: llm.ToolFunction{Name: name, Arguments: args},
			}}}
		}
		switch {
		case rejected && writesDone(req) >= 2:
			return toolcall("fin2", "finish", `{"summary":"clean now"}`), nil
		case rejected:
			// Rewrite the file without the markers, then try finish again.
			return toolcall("w2", "write_file", `{"path":"x.txt","content":"clean"}`), nil
		case writesDone(req) >= 1:
			// First finish attempt over a conflicted tree: must be blocked.
			return toolcall("fin1", "finish", `{"summary":"too early"}`), nil
		default:
			return toolcall("w1", "write_file", conflictContent), nil
		}
	}}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.Goal = "blocked gate"
	cfg.Stream = false
	cfg.NativeTools = true
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	sb, err := tools.NewSandbox(work)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := o.runWorker(context.Background(), sb, o.tree.GetRoot(), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "clean now" {
		t.Fatalf("summary = %q", summary)
	}
	// The rejection came back as a role:tool message bound to the finish call.
	bound := false
	for _, req := range client.Requests {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && m.Name == "finish" && strings.Contains(m.Content, "finish REJECTED") {
				bound = true
			}
		}
	}
	if !bound {
		t.Fatal("finish rejection was not delivered as a tool message")
	}
}

func writesDone(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool && m.Name == "write_file" {
			n++
		}
	}
	return n
}

func TestProjectContextInjectsInputs(t *testing.T) {
	o, sb := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	if err := sb.WriteFile("in.txt", "input body\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("b.txt", "b body\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("c.txt", "c body\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("d.txt", "d body\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("e.txt", "e body\n"); err != nil {
		t.Fatal(err)
	}
	node := &models.TaskNode{
		ID:          "n1",
		Title:       "task with inputs",
		Description: "desc",
		Contract: models.ContractSpec{
			Inputs: []string{
				"not a path",                                 // skipped: not path-shaped
				"missing/input.txt",                          // skipped: unreadable
				"in.txt", "b.txt", "c.txt", "d.txt", "e.txt", // only first 4 injected
			},
		},
	}
	ctx := o.projectContext(sb, node, "prev boom")
	for _, want := range []string{"### in.txt", "input body", "### b.txt", "### c.txt", "### d.txt", "Previous attempt failed", "prev boom"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("project context missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "### e.txt") {
		t.Error("more than 4 inputs must not be injected")
	}
	if strings.Contains(ctx, "### missing/input.txt") {
		t.Error("unreadable input must be skipped")
	}
}

// --- guided.go -------------------------------------------------------------

// newRunnableGuider builds a guider whose goal marks the root COMPOUND, plus
// the scripted IO to drive it.
func newRunnableGuider(t *testing.T, client llm.Client) (*Guider, *chanGuided) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.Goal = "build a cli tool" // runnable → compound root
	cfg.RetryMinInterval = time.Millisecond
	cfg.RetryMaxInterval = 2 * time.Millisecond
	o, err := NewFromGoal(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	gio := newChanGuided()
	return NewGuider(o, gio), gio
}

// cancelOnPrintIO cancels a context the first time a Printf matching needle
// fires. It makes "cancel while the controller sits in a specific select"
// deterministic instead of racing the scheduler.
type cancelOnPrintIO struct {
	inner  *chanGuided
	needle string
	cancel context.CancelFunc
}

func (c *cancelOnPrintIO) Lines() <-chan string { return c.inner.Lines() }
func (c *cancelOnPrintIO) Printf(format string, a ...any) {
	if strings.Contains(format, c.needle) {
		c.cancel()
	}
	c.inner.Printf(format, a...)
}

func TestNewStdioGuidedStreams(t *testing.T) {
	in := strings.NewReader("first\nsecond\n")
	var out strings.Builder
	g := NewStdioGuided(in, &out)
	got := make([]string, 0, 2)
	for line := range g.Lines() {
		got = append(got, line)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("lines = %v", got)
	}
	g.Printf("hello %s\n", "io")
	if !strings.Contains(out.String(), "hello io") {
		t.Fatalf("Printf output = %q", out.String())
	}
}

func TestGuidedRun_PlanningCanceled(t *testing.T) {
	g, _ := newRunnableGuider(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled planning must surface the ctx error, got %v", err)
	}
}

func TestReview_ReplanErrorPropagates(t *testing.T) {
	// An adjustment whose replan fails (canceled context) must return the
	// error from review rather than looping forever.
	g, cg := newRunnableGuider(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.io = &cancelOnPrintIO{inner: cg, needle: "revising plan", cancel: cancel}
	cg.ch <- "make it smaller"

	err := g.review(ctx)
	if err == nil || err == errStdinClosed {
		t.Fatalf("replan failure must propagate, got %v", err)
	}
}

func TestReview_ContextCanceled(t *testing.T) {
	g, _ := newRunnableGuider(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.review(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled review must return ctx.Err, got %v", err)
	}
}

func TestExecute_CanceledAtStart(t *testing.T) {
	g, _ := newRunnableGuider(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled execute must return ctx.Err, got %v", err)
	}
}

func TestExecute_DecomposesPendingCompound(t *testing.T) {
	// A pending compound discovered during execution is decomposed inline,
	// its leaf runs, and the goal completes without another planning round.
	plan := `{"is_atomic": true, "reason": "tiny", "contract": {}, "dod": {"commands": ["true"]}, "subtasks": []}`
	client := &scriptedClient{responses: []string{plan, `{"action":"finish","args":{"summary":"sub done"}}`}}
	g, _ := newRunnableGuider(t, client)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "sub", "Sub", "desc", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}

	if err := g.execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	sub, _ := g.o.tree.CloneNode("sub")
	if sub.Type != models.NodeTypeLeaf || sub.State != models.TaskStateCompleted {
		t.Fatalf("compound should have been decomposed and completed: %+v", sub)
	}
	if !strings.Contains(g.io.(*chanGuided).printed.String(), "decomposing %s") {
		t.Fatal("execute never announced the decomposition")
	}
}

// noLeafReadyGuider builds a compound root with one stuck (RUNNING) leaf, so
// execute reaches the "no leaf is ready" interaction branch.
func noLeafReadyGuider(t *testing.T, lines ...string) (*Guider, *chanGuided) {
	t.Helper()
	g, cg := newRunnableGuider(t, &llm.ScriptedClient{})
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "stuck", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := g.o.sched.UpdateNodeState("1.1", models.TaskStateRunning, ""); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		cg.ch <- l
	}
	return g, cg
}

func TestExecute_NoLeafReadyLineHandling(t *testing.T) {
	g, cg := noLeafReadyGuider(t, "/bogus", "pause")
	err := g.execute(context.Background())
	if err != ErrPaused {
		t.Fatalf("want ErrPaused, got %v", err)
	}
	printed := cg.printed.String()
	if !strings.Contains(printed, "no leaf is ready") {
		t.Fatalf("idle prompt never shown:\n%s", printed)
	}
}

func TestExecute_NoLeafReadyEOF(t *testing.T) {
	g, cg := noLeafReadyGuider(t)
	cg.close()
	if err := g.execute(context.Background()); err != errStdinClosed {
		t.Fatalf("want errStdinClosed, got %v", err)
	}
}

func TestExecute_NoLeafReadyCanceled(t *testing.T) {
	g, cg := noLeafReadyGuider(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.io = &cancelOnPrintIO{inner: cg, needle: "no leaf is ready", cancel: cancel}
	if err := g.execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestGuidedRun_EOFDuringLeafReturnsCleanly(t *testing.T) {
	// EOF while a leaf is mid-run: the run ends without error, the plan was
	// checkpointed, and the leaf is runnable again on resume.
	started := make(chan struct{}, 1)
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{Content: atomicFilePlan}, nil
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.Goal = "write x.txt"
	o, err := NewFromGoal(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	cg := newChanGuided("/approve")
	g := NewGuider(o, cg)
	done := make(chan error, 1)
	go func() { done <- g.Run(context.Background()) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("leaf never started")
	}
	cg.close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EOF mid-leaf must be a clean exit, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after EOF")
	}
	root := o.tree.GetRoot()
	if root.State != models.TaskStatePending {
		t.Fatalf("interrupted leaf must stay pending, got %s", root.State)
	}
}

// gatedClient blocks the first model call until released, then finishes.
type gatedClient struct {
	started chan struct{}
	release chan struct{}
}

func (c *gatedClient) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Response{Content: `{"thought":"done","action":"finish","args":{"summary":"released"}}`}, nil
}

// printingIO wraps a GuidedIO and keeps the fully formatted output, unlike
// chanGuided which records only format strings.
type printingIO struct {
	inner GuidedIO
	mu    sync.Mutex
	buf   strings.Builder
}

func (p *printingIO) Lines() <-chan string { return p.inner.Lines() }
func (p *printingIO) Printf(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(&p.buf, format, a...)
}
func (p *printingIO) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func TestRunLeafInteractive_QueuesNotesAndAppliesAfterLeaf(t *testing.T) {
	g, cg := newRunnableGuider(t, &llm.ScriptedClient{})
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "long leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")

	capIO := &printingIO{inner: cg}
	g.io = capIO

	gc := &gatedClient{started: make(chan struct{}, 1), release: make(chan struct{})}
	g.o.llm = gc

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-gc.started
	cg.ch <- "free text note" // queued, not a plan edit
	cg.ch <- "/nope"          // queued too, and errors when applied
	close(gc.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("leaf should finish after release, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLeafInteractive did not return after release")
	}
	printed := capIO.String()
	if !strings.Contains(printed, "note queued") {
		t.Fatalf("free text was not queued as a note:\n%s", printed)
	}
	if !strings.Contains(printed, "unknown command") {
		t.Fatalf("queued /nope error was not reported:\n%s", printed)
	}
	// The free-text note became a new leaf.
	if !g.o.tree.HasNode("free_text_note") {
		t.Fatal("queued free text should have been added as a leaf")
	}
}

func TestRunLeafInteractive_EOFWhileAnsweringAsk(t *testing.T) {
	g, cg := newRunnableGuider(t, &scriptedClient{responses: []string{
		`{"thought":"need input","action":"ask","args":{"question":"port?"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"finished"}}`,
	}})
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "asking", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")
	askDelivered := installAskHook(g)

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-askDelivered
	cg.close()

	select {
	case err := <-done:
		if err != errStdinClosed {
			t.Fatalf("want errStdinClosed, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EOF while answering did not end the leaf")
	}
}

func TestRunLeafInteractive_CancelWhileAnsweringAsk(t *testing.T) {
	g, _ := newRunnableGuider(t, &scriptedClient{responses: []string{
		`{"thought":"need input","action":"ask","args":{"question":"port?"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"finished"}}`,
	}})
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "asking", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")
	askDelivered := installAskHook(g)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(ctx, leaf) }()

	<-askDelivered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel while answering did not end the leaf")
	}
}

func TestUnblockAskerWakesWorker(t *testing.T) {
	g, _ := newRunnableGuider(t, &llm.ScriptedClient{})
	answer := make(chan string, 1)
	go func() {
		g.askCh <- "which port?" // the worker's askHook send
		answer <- <-g.answerCh   // ...then blocks on the reply
	}()
	if q := <-g.askCh; q != "which port?" {
		t.Fatalf("unexpected question %q", q)
	}
	g.unblockAsker("test closed")
	select {
	case ans := <-answer:
		if !strings.Contains(ans, "test closed") {
			t.Fatalf("unexpected unblock reason %q", ans)
		}
	case <-time.After(time.Second):
		t.Fatal("unblockAsker did not wake the blocked asker")
	}
}

func TestHandleExecLine_FreeTextAddsLeaf(t *testing.T) {
	g, _ := newRunnableGuider(t, &llm.ScriptedClient{})
	if err := g.handleExecLine(context.Background(), "please add more tests"); err != nil {
		t.Fatal(err)
	}
	if !g.o.tree.HasNode("please_add_more_tests") {
		t.Fatal("free text during idle should create a leaf")
	}
}

// --- decomposer.go ---------------------------------------------------------

func TestDecompose_MaxDepthForcesLeaf(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: `{"is_atomic": false, "subtasks": [{"id": "a"}, {"id": "b"}]}`}, nil
	}}
	o, _ := newWorkerOrchestrator(t, client)
	if _, err := o.tree.AddChild(o.tree.RootID, "deep", "Deep", "desc", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("deep", func(n *models.TaskNode) error {
		n.Depth = o.cfg.MaxDepth
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	node, _ := o.tree.CloneNode("deep")
	if err := o.decompose(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	live, _ := o.tree.CloneNode("deep")
	if live.Type != models.NodeTypeLeaf {
		t.Fatalf("max-depth node must be forced to a leaf, got %s", live.Type)
	}
	if len(client.Requests) != 0 {
		t.Fatal("forced leaf must not call the model")
	}
}

func TestDecompose_PlanRepairForcedAtomic(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"is_atomic": false, "subtasks": [{"id": "a"}, {"id": "b"}]}`,
	}}
	o, _ := newWorkerOrchestrator(t, client)
	o.cfg.DecomposeTries = 1
	if _, err := o.tree.AddChild(o.tree.RootID, "deep", "Deep", "desc", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("deep", func(n *models.TaskNode) error {
		n.Depth = o.cfg.MaxDepth
		n.ErrorMsg = planRepairMarker + " expected_output never printed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	node, _ := o.tree.CloneNode("deep")
	if err := o.decompose(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	live, _ := o.tree.CloneNode("deep")
	if live.Type != models.NodeTypeLeaf {
		t.Fatalf("plan repair at max depth must stay a leaf, got %s", live.Type)
	}
	if len(live.ChildrenIDs) != 0 {
		t.Fatal("plan repair at max depth must not grow the tree")
	}
}

func TestDecompose_InvalidJSONThenSuccess(t *testing.T) {
	plan := `{"is_atomic": true, "contract": {}, "dod": {"commands": ["true"]}, "subtasks": []}`
	client := &scriptedClient{responses: []string{"{not json", plan}}
	o, _ := newWorkerOrchestrator(t, client)
	o.cfg.DecomposeTries = 3

	root := o.tree.GetRoot()
	if err := o.decompose(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", client.calls)
	}
	found := false
	for _, m := range client.lastMsgs {
		if strings.Contains(m.Content, "previous JSON was invalid") {
			found = true
		}
	}
	if !found {
		t.Fatal("retry must quote the previous failure to the model")
	}
}

func TestDecompose_MissingNodeFails(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	err := o.decompose(context.Background(), &models.TaskNode{ID: "ghost-node"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("decomposing a missing node must fail, got %v", err)
	}
}

func TestDecomposeUserPromptSections(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	if err := os.WriteFile(filepath.Join(o.cfg.WorkDir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p := o.decomposeUserPrompt(&models.TaskNode{ID: "root", Title: "T", Description: "D"})
	if !strings.Contains(p, "Detected toolchain") {
		t.Errorf("go workspace should announce the toolchain:\n%s", p)
	}

	p = o.decomposeUserPrompt(&models.TaskNode{ID: "root", ErrorMsg: planRepairMarker + " bad expected_output"})
	if !strings.Contains(p, "DIAGNOSE THE PLAN FIRST") {
		t.Errorf("plan-repair failures need the diagnose framing:\n%s", p)
	}

	p = o.decomposeUserPrompt(&models.TaskNode{ID: "root", ErrorMsg: "verify failed: exit 1"})
	if !strings.Contains(p, "you MUST split smaller") {
		t.Errorf("ordinary failures need the split framing:\n%s", p)
	}

	if _, err := o.tree.AddChild(o.tree.RootID, "kid", "Kid", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	p = o.decomposeUserPrompt(&models.TaskNode{ID: "kid", ParentID: o.tree.RootID})
	if !strings.Contains(p, "Parent:") {
		t.Errorf("parent context missing:\n%s", p)
	}
}

func TestApplyDecompose_MergesSingleSubtask(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})

	// Empty top-level contract/DoD: the single subtask's spec is promoted.
	parsed := decomposeResult{
		IsAtomic: true,
		Subtasks: []decomposeChild{{
			ID: "s", Title: "S",
			Contract: models.ContractSpec{Outputs: []string{"sub.txt"}},
			DoD:      models.DoD{Commands: []string{"test -f sub.txt"}},
		}},
	}
	if err := o.applyDecompose(o.tree.GetRoot(), parsed); err != nil {
		t.Fatal(err)
	}
	live, _ := o.tree.CloneNode(o.tree.RootID)
	if len(live.Contract.Outputs) != 1 || live.Contract.Outputs[0] != "sub.txt" {
		t.Fatalf("subtask contract not merged: %+v", live.Contract)
	}
	if len(live.DoD.Commands) != 1 || live.DoD.Commands[0] != "test -f sub.txt" {
		t.Fatalf("subtask DoD not merged: %+v", live.DoD)
	}

	// Non-empty top-level spec wins over the subtask's.
	o2, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	parsed2 := decomposeResult{
		IsAtomic: true,
		Contract: models.ContractSpec{Outputs: []string{"top.txt"}},
		DoD:      models.DoD{Commands: []string{"true"}},
		Subtasks: []decomposeChild{{
			ID: "s", Title: "S",
			Contract: models.ContractSpec{Outputs: []string{"sub.txt"}},
			DoD:      models.DoD{Commands: []string{"test -f sub.txt"}},
		}},
	}
	if err := o2.applyDecompose(o2.tree.GetRoot(), parsed2); err != nil {
		t.Fatal(err)
	}
	live2, _ := o2.tree.CloneNode(o2.tree.RootID)
	if len(live2.Contract.Outputs) != 1 || live2.Contract.Outputs[0] != "top.txt" {
		t.Fatalf("top-level contract must win: %+v", live2.Contract)
	}
	if live2.DoD.Commands[0] != "true" {
		t.Fatalf("top-level DoD must win: %+v", live2.DoD)
	}
}

func TestApplyDecompose_NonAtomicSingleSubtaskRejected(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	parsed := decomposeResult{
		IsAtomic: false,
		Subtasks: []decomposeChild{{ID: "only", Title: "Only"}},
	}
	err := o.applyDecompose(o.tree.GetRoot(), parsed)
	if err == nil || !strings.Contains(err.Error(), "at least 2 subtasks") {
		t.Fatalf("single-subtask non-atomic split must be rejected, got %v", err)
	}
}

func TestDecompose_RetriesAfterApplyFailure(t *testing.T) {
	// applyDecompose rejections (here: one subtask non-atomic) feed back into
	// the retry loop; the corrected second answer succeeds.
	good := `{"is_atomic": true, "contract": {}, "dod": {"commands": ["true"]}, "subtasks": []}`
	bad := `{"is_atomic": false, "subtasks": [{"id": "only", "title": "Only"}]}`
	client := &scriptedClient{responses: []string{bad, good}}
	o, _ := newWorkerOrchestrator(t, client)
	o.cfg.DecomposeTries = 3

	root := o.tree.GetRoot()
	if err := o.decompose(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("expected the apply failure to trigger one retry, got %d calls", client.calls)
	}
}

func TestApplyDecompose_TruncatesExtraSubtasks(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	o.cfg.MaxSubtasks = 2
	parsed := decomposeResult{
		IsAtomic: false,
		Subtasks: []decomposeChild{
			{ID: "s1", Title: "S1"}, {ID: "s2", Title: "S2"}, {ID: "s3", Title: "S3"},
		},
	}
	if err := o.applyDecompose(o.tree.GetRoot(), parsed); err != nil {
		t.Fatal(err)
	}
	kids, err := o.tree.GetChildren(o.tree.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("subtasks must be capped at MaxSubtasks, got %d", len(kids))
	}
}

func TestApplyDecompose_CompoundChild(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	parsed := decomposeResult{
		IsAtomic: false,
		Subtasks: []decomposeChild{
			{ID: "kid1", Title: "Leaf part", Type: "leaf"},
			{ID: "kid2", Title: "Bigger part", Type: "compound"},
		},
	}
	if err := o.applyDecompose(o.tree.GetRoot(), parsed); err != nil {
		t.Fatal(err)
	}
	kid2, _ := o.tree.CloneNode("kid2")
	if kid2.Type != models.NodeTypeCompound {
		t.Fatalf("compound child type lost: %s", kid2.Type)
	}
	kid1, _ := o.tree.CloneNode("kid1")
	if kid1.Type != models.NodeTypeLeaf || len(kid1.DoD.Commands) == 0 {
		t.Fatalf("leaf child must keep default DoD commands: %+v", kid1)
	}
}

func TestApplyDecompose_CycleResetsChildren(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	parsed := decomposeResult{
		IsAtomic: false,
		Subtasks: []decomposeChild{
			{ID: "a", Title: "A", Contract: models.ContractSpec{Dependencies: []string{"b"}}},
			{ID: "b", Title: "B", Contract: models.ContractSpec{Dependencies: []string{"a"}}},
		},
	}
	err := o.applyDecompose(o.tree.GetRoot(), parsed)
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("dependency cycle must fail validation, got %v", err)
	}
	if o.tree.HasNode("a") || o.tree.HasNode("b") {
		t.Fatal("failed validation must roll the children back")
	}
}

func TestConvertToLeaf_WithChildrenFails(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	if _, err := o.tree.AddChild(o.tree.RootID, "kid", "Kid", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	err := o.convertToLeaf(o.tree.RootID, models.ContractSpec{}, models.DoD{})
	if err == nil || !strings.Contains(err.Error(), "with children") {
		t.Fatalf("converting a compound with children must fail, got %v", err)
	}
}

func TestNormalizeChildrenDedup(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	tree := o.tree
	if _, err := tree.AddChild(tree.RootID, "taken", "Taken", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.AddChild(tree.RootID, "parent1", "Parent", "desc", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	parent, _ := tree.CloneNode("parent1")

	raw := []decomposeChild{
		{ID: "parent1", Title: "Clashes with parent"},  // parent id → renamed
		{ID: "taken", Title: "Clashes with tree node"}, // existing node → renamed
		{ID: "dup", Title: "First dup"},
		{ID: "dup", Title: "Second dup"}, // sibling clash → renamed
		{ID: "", Title: ""},              // untitled fallback + title-derived id
		{ID: "Alpha", Title: "Alpha"},
		{ID: "b", Title: "B", Contract: models.ContractSpec{Dependencies: []string{"ALPHA", "ghost", "b"}}},
	}
	out, err := normalizeChildren(raw, tree, parent)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, ch := range out {
		if ids[ch.ID] {
			t.Fatalf("duplicate id after normalization: %s", ch.ID)
		}
		ids[ch.ID] = true
		if ch.ID == "parent1" || ch.ID == "taken" || ch.ID == "" {
			t.Fatalf("clashing id survived: %q", ch.ID)
		}
	}
	if !ids["untitled"] {
		t.Errorf("empty title/id child should become untitled, got %v", ids)
	}
	for _, ch := range out {
		if ch.ID == "b" {
			// Lowercase mapping resolved ALPHA to the alpha child; the
			// unknown ghost was dropped; the explicit self-match dropped too.
			if len(ch.Contract.Dependencies) != 1 || ch.Contract.Dependencies[0] != "alpha" {
				t.Fatalf("dependency mapping wrong: %+v", ch.Contract.Dependencies)
			}
		}
		if ch.ID == "alpha" && len(ch.Contract.Dependencies) != 0 {
			t.Fatalf("alpha should carry no dependencies: %+v", ch.Contract.Dependencies)
		}
		if ch.Title == "" || ch.Description == "" {
			t.Fatalf("title/description must never stay empty: %+v", ch)
		}
	}
}

func TestFormatDoDShowsExpectedOutput(t *testing.T) {
	s := formatDoD(models.DoD{
		Description:    "works",
		Commands:       []string{"true"},
		ExpectedOutput: "hello world",
	})
	for _, want := range []string{"description: works", "- true", "expected_output: hello world"} {
		if !strings.Contains(s, want) {
			t.Errorf("formatDoD missing %q:\n%s", want, s)
		}
	}
}

// --- events.go -------------------------------------------------------------

func TestEventRecorderNilSafety(t *testing.T) {
	var r *EventRecorder
	r.AddSecret("some-secret-value") // no panic
	r.Record("kind", "node", nil)    // no panic

	ch, cancel := r.Subscribe()
	if ch == nil {
		t.Fatal("nil recorder must still return a usable channel shape")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("nil-recorder subscription must close on cancel")
	}
	if r.SubscriberCount() != 0 {
		t.Fatal("nil recorder has no subscribers")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("nil Close = %v", err)
	}
}

func TestScrubSecretsValueTypes(t *testing.T) {
	sec := "topsecret-value"
	in := map[string]any{
		"plain":      "carry " + sec,
		sec + "-key": "value under a secret-named key",
		"nested":     []any{[]any{"deep " + sec}, 7},
		"raw":        []byte(`{"blob":"` + sec + `"}`),
		"num":        42,
		"unmatched":  "nothing here",
	}
	out := scrubSecrets(in, []string{sec}).(map[string]any)
	if out["plain"] != "carry "+secretMask {
		t.Errorf("string value not scrubbed: %v", out["plain"])
	}
	if _, gone := out[sec+"-key"]; gone {
		t.Errorf("secret-named key must be renamed")
	}
	if out[secretMask+"-key"] != "value under a secret-named key" {
		t.Errorf("renamed key missing: %v", out)
	}
	nested := out["nested"].([]any)
	if nested[0].([]any)[0] != "deep "+secretMask {
		t.Errorf("nested slice not scrubbed: %v", nested)
	}
	if !bytes.Contains(out["raw"].([]byte), []byte(secretMask)) {
		t.Errorf("raw bytes payload not scrubbed: %s", out["raw"].([]byte))
	}
	if out["num"] != 42 {
		t.Errorf("non-string values must pass through, got %v", out["num"])
	}
	if out["unmatched"] != "nothing here" {
		t.Errorf("unmatched string must be untouched, got %v", out["unmatched"])
	}
}

func TestNewEventRecorderUnwritablePath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEventRecorder(filepath.Join(blocker, "events.jsonl")); err == nil {
		t.Fatal("a file-shaped parent directory must fail recorder creation")
	}
}

func TestTrimPartialLineCases(t *testing.T) {
	dir := t.TempDir()

	// Empty file: nothing to trim.
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	trimPartialLine(empty)
	if data, err := os.ReadFile(empty); err != nil || len(data) != 0 {
		t.Fatalf("empty file changed: %q %v", data, err)
	}

	// No newline at all: the whole torn content goes away.
	torn := filepath.Join(dir, "torn.jsonl")
	if err := os.WriteFile(torn, []byte("half-written event"), 0644); err != nil {
		t.Fatal(err)
	}
	trimPartialLine(torn)
	if data, err := os.ReadFile(torn); err != nil || len(data) != 0 {
		t.Fatalf("newline-less file must be truncated to 0, got %q %v", data, err)
	}

	// Torn tail after complete lines: keep only the complete prefix.
	mixed := filepath.Join(dir, "mixed.jsonl")
	if err := os.WriteFile(mixed, []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":3"), 0644); err != nil {
		t.Fatal(err)
	}
	trimPartialLine(mixed)
	data, err := os.ReadFile(mixed)
	if err != nil || string(data) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Fatalf("torn tail not trimmed correctly: %q %v", data, err)
	}
}

func TestHighestEventSeqMissingFile(t *testing.T) {
	if got := highestEventSeq(filepath.Join(t.TempDir(), "nope.jsonl")); got != 0 {
		t.Fatalf("missing file must give seq 0, got %d", got)
	}
}

func TestRecordUnmarshalableFieldSkipsWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	// A channel cannot be marshaled: the event is dropped, not written.
	rec.Record("bad", "", map[string]any{"ch": make(chan int)})
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 0 {
		t.Fatalf("unmarshalable event must not be written, got %q %v", data, err)
	}
}

func TestClipAndTailClip(t *testing.T) {
	if got := clip("  short  ", 10); got != "short" {
		t.Errorf("clip short = %q", got)
	}
	long := strings.Repeat("x", 50)
	if got := clip(long, 10); !strings.HasPrefix(got, "xxxxxxxxxx...(+") {
		t.Errorf("clip long = %q", got)
	}
	if got := tailClip(long, 10); !strings.HasPrefix(got, "(+40 chars)...\n") {
		t.Errorf("tailClip = %q", got)
	}
	if got := tailClip(" tiny ", 10); got != "tiny" {
		t.Errorf("tailClip short = %q", got)
	}
}

func TestSummarizeMessagesShapes(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "system prompt"},
		{Role: llm.RoleUser, Content: "the task"},
	}
	// Named message with tool calls exercises the name/tool_calls fields.
	for i := 0; i < 30; i++ {
		msgs = append(msgs, llm.Message{
			Role: llm.RoleAssistant, Name: "worker", Content: strings.Repeat("x", 300),
			ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "write_file"}}},
		})
	}
	out := summarizeMessages(msgs)
	// 2 head messages + 1 omitted note + 22 tail messages.
	if len(out) != 25 {
		t.Fatalf("expected 2+1+22 summaries, got %d", len(out))
	}
	note, ok := out[2]["note"].(string)
	if !ok || !strings.Contains(note, "earlier messages omitted") {
		t.Fatalf("missing omitted-note: %v", out[2])
	}
	sample := out[3]
	if sample["name"] != "worker" {
		t.Errorf("message name not summarized: %v", sample)
	}
	calls, ok := sample["tool_calls"].([]string)
	if !ok || len(calls) != 1 || calls[0] != "write_file" {
		t.Errorf("tool calls not summarized: %v", sample["tool_calls"])
	}
	if p, _ := sample["preview"].(string); !strings.HasSuffix(p, "...(+100 chars)") {
		t.Errorf("preview not clipped: %q", p)
	}
}

func TestLLMCallRecordsToolCallsAndFinish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}
	rec.LLMCall("worker", "n1", req, &llm.Response{
		Content:   "working",
		ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "finish"}}},
		Usage:     llm.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
		Finish:    "stop",
	}, time.Now(), nil)
	rec.LLMCall("worker", "n1", req, nil, time.Now(), errors.New("boom"))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	events := readEventStream(t, path)
	calls := eventsOfKind(events, "llm_call")
	if len(calls) != 2 {
		t.Fatalf("expected 2 llm_call events, got %d", len(calls))
	}
	if names, _ := calls[0]["resp_tool_calls"].([]any); len(names) != 1 || names[0] != "finish" {
		t.Errorf("resp tool calls missing: %v", calls[0])
	}
	if calls[0]["finish_reason"] != "stop" {
		t.Errorf("finish reason missing: %v", calls[0])
	}
	if calls[1]["error"] != "boom" {
		t.Errorf("error not recorded: %v", calls[1])
	}
}

func TestSummarizeEventsUnreadablePath(t *testing.T) {
	if _, err := SummarizeEvents(t.TempDir()); err == nil {
		t.Fatal("a directory path must surface a read error")
	}
}

func TestEventTimeMalformed(t *testing.T) {
	if _, ok := eventTime(map[string]any{}); ok {
		t.Error("missing ts must not parse")
	}
	if _, ok := eventTime(map[string]any{"ts": "not-a-time"}); ok {
		t.Error("bad ts must not parse")
	}
	ts, ok := eventTime(map[string]any{"ts": "2026-01-02T03:04:05Z"})
	if !ok || ts.IsZero() {
		t.Errorf("valid ts must parse, got %v ok=%v", ts, ok)
	}
}

func TestToolCallRecordsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.ToolCall("n1", "run_bash", map[string]any{"command": "false"}, "boom output", errors.New("exit 1"), time.Now())
	rec.ToolCall("n1", "write_file", map[string]any{"path": "a.txt"}, "wrote", nil, time.Now())
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	events := readEventStream(t, path)
	calls := eventsOfKind(events, "tool_call")
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool_call events, got %d", len(calls))
	}
	if ok, _ := calls[0]["ok"].(bool); ok || calls[0]["error"] != "exit 1" {
		t.Errorf("failed tool call not recorded: %v", calls[0])
	}
	if ok, _ := calls[1]["ok"].(bool); !ok {
		t.Errorf("successful tool call not recorded: %v", calls[1])
	}
	if !strings.Contains(calls[1].str("args"), "a.txt") {
		t.Errorf("args not recorded: %v", calls[1])
	}
}

// --- repl.go ---------------------------------------------------------------

// failingClient always errors, for exercising transient-failure paths.
type failingClient struct {
	err error
}

func (c *failingClient) Chat(context.Context, llm.Request) (*llm.Response, error) {
	return nil, c.err
}

func TestNewREPLRejectsBadWorkdir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(cfg.WorkDir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewREPL(cfg, &llm.ScriptedClient{}); err == nil {
		t.Fatal("a file-shaped workdir must fail NewREPL")
	}
}

func TestREPLGoalTurnErrorEndsRun(t *testing.T) {
	repl := newTestREPL(t, &failingClient{err: errors.New("model down")})
	repl.o.cfg.Goal = "do it"
	err := repl.Run(context.Background(), &scriptedIO{lines: []string{"/exit"}})
	if err == nil || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("goal turn failure must end the run with the error, got %v", err)
	}
}

func TestREPLCanceledBeforeLoop(t *testing.T) {
	repl := newTestREPL(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repl.Run(ctx, &scriptedIO{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled REPL must return ctx.Err, got %v", err)
	}
}

// errReadIO fails every ReadLine, simulating a detached terminal.
type errReadIO struct {
	out strings.Builder
}

func (e *errReadIO) ReadLine() (string, error) { return "", errors.New("tty detached") }
func (e *errReadIO) Printf(format string, a ...any) {
	fmt.Fprintf(&e.out, format, a...)
}

func TestREPLReadLineErrorEndsRun(t *testing.T) {
	repl := newTestREPL(t, &llm.ScriptedClient{})
	err := repl.Run(context.Background(), &errReadIO{})
	if err == nil || !strings.Contains(err.Error(), "tty detached") {
		t.Fatalf("read error must end the run, got %v", err)
	}
}

func TestREPLSkipsEmptyLines(t *testing.T) {
	repl := newTestREPL(t, &scriptedClient{responses: []string{""}})
	mio := &scriptedIO{lines: []string{"", "   ", "/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	// Only the banner and prompts printed; no turn was dispatched.
	if strings.Count(mio.out.String(), "\n> ") < 3 {
		t.Fatalf("empty lines should re-prompt:\n%s", mio.out.String())
	}
}

func TestREPLTransientErrorContinues(t *testing.T) {
	calls := 0
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient 502")
		}
		return &llm.Response{Content: `{"thought":"ok","action":"respond","args":{"message":"recovered"}}`}, nil
	}}
	repl := newTestREPL(t, client)
	// The transient error comes from an interactive turn: the goal turn is not
	// set, so the loop-level recovery path (print and continue) applies.
	mio := &scriptedIO{lines: []string{"say hi", "try again", "/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	out := mio.out.String()
	if !strings.Contains(out, "transient 502") || !strings.Contains(out, "recovered") {
		t.Fatalf("transient error must not kill the session:\n%s", out)
	}
}

func TestREPLTurnCanceledAtStart(t *testing.T) {
	repl := newTestREPL(t, &llm.ScriptedClient{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repl.turn(ctx, "hi", &scriptedIO{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled turn must return ctx.Err, got %v", err)
	}
}

func TestREPLTurnErrorWithCanceledContext(t *testing.T) {
	// A model failure after cancellation ends the session instead of being
	// swallowed as a transient error.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		cancel()
		return nil, errors.New("dying connection")
	}}
	repl := newTestREPL(t, client)
	mio := &scriptedIO{lines: []string{"go"}}
	err := repl.Run(ctx, mio)
	if err == nil || !strings.Contains(err.Error(), "dying connection") {
		t.Fatalf("canceled turn must end the run with the error, got %v", err)
	}
}

func TestREPLTurnChatError(t *testing.T) {
	repl := newTestREPL(t, &failingClient{err: errors.New("model down")})
	err := repl.turn(context.Background(), "hi", &scriptedIO{})
	if err == nil || !strings.Contains(err.Error(), "foreman request failed") {
		t.Fatalf("chat errors must be wrapped, got %v", err)
	}
}

func TestREPLUnknownActionCorrected(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"thought":"hmm","action":"dance","args":{}}`,
		`{"thought":"ok","action":"respond","args":{"message":"recovered"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "do it"
	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mio.out.String(), "recovered") {
		t.Fatalf("unknown action must be corrected then answered:\n%s", mio.out.String())
	}
}

func TestREPLBadDispatchCorrected(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"action":"dispatch","args":{"tasks":"not-a-list"}}`,
		`{"action":"dispatch","args":{"tasks":[]}}`,
		`{"action":"respond","args":{"message":"gave an answer"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "do it"
	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if repl.leaves != 0 {
		t.Fatalf("bad dispatches must not run leaves, got %d", repl.leaves)
	}
	if !strings.Contains(mio.out.String(), "gave an answer") {
		t.Fatalf("empty dispatch must be corrected:\n%s", mio.out.String())
	}
}

func TestREPLLeafErrorSurfacesInResult(t *testing.T) {
	calls := 0
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		calls++
		switch calls {
		case 1: // foreman dispatches one task
			return &llm.Response{Content: `{"action":"dispatch","args":{"tasks":[{"title":"do","description":"Do the thing"}]}}`}, nil
		case 2: // the leaf worker's model call fails
			return nil, errors.New("worker model down")
		default: // foreman reports the leaf failure
			return &llm.Response{Content: `{"action":"respond","args":{"message":"leaf failed as expected"}}`}, nil
		}
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "go"
	mio := &scriptedIO{}
	if err := repl.turn(context.Background(), "go", mio); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mio.out.String(), "leaf failed as expected") {
		t.Fatalf("foreman reply missing:\n%s", mio.out.String())
	}
	if len(repl.summaries) != 1 || !strings.Contains(repl.summaries[0], "ERROR: worker model down") {
		t.Fatalf("leaf error must be recorded in the turn summary: %+v", repl.summaries)
	}
}

func TestREPLDispatchRoundsCapped(t *testing.T) {
	// A foreman that keeps dispatching is stopped after replMaxDispatch rounds.
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: `{"action":"dispatch","args":{"tasks":[{"title":"again","description":"Keep going"}]}}`}, nil
	}}
	repl := newTestREPL(t, client)
	mio := &scriptedIO{}
	if err := repl.turn(context.Background(), "go", mio); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mio.out.String(), "stopped after 10 dispatch rounds") {
		t.Fatalf("dispatch cap not enforced:\n%s", mio.out.String())
	}
	if repl.leaves != replMaxDispatch {
		t.Fatalf("expected %d leaf runs, got %d", replMaxDispatch, repl.leaves)
	}
}

func TestREPLLeafNodeCarriesWorkLog(t *testing.T) {
	repl := newTestREPL(t, &llm.ScriptedClient{})
	repl.summaries = []string{"earlier: fixed the parser"}
	node := repl.newLeafNode(foremanTask{title: "next", description: "do next"})
	if !strings.Contains(node.Description, "Recent work log") ||
		!strings.Contains(node.Description, "earlier: fixed the parser") {
		t.Fatalf("work log missing from leaf description: %q", node.Description)
	}
	if node.ID != "leaf_1" || node.Title != "next" {
		t.Fatalf("unexpected node: %+v", node)
	}
}

func TestRecordTurnCapsSummaries(t *testing.T) {
	repl := newTestREPL(t, &llm.ScriptedClient{})
	for i := 0; i < 10; i++ {
		repl.recordTurn(fmt.Sprintf("u%d", i), []string{"leaf result"}, fmt.Sprintf("reply %d", i))
	}
	if len(repl.summaries) != replMaxSummaries {
		t.Fatalf("summaries must cap at %d, got %d", replMaxSummaries, len(repl.summaries))
	}
	if !strings.Contains(repl.summaries[0], "u2") {
		t.Fatalf("oldest entries must be dropped first: %q", repl.summaries[0])
	}
	if !strings.Contains(repl.summaries[0], "leaf: leaf result") ||
		!strings.Contains(repl.summaries[0], "reply: reply 2") {
		t.Fatalf("summary shape wrong: %q", repl.summaries[0])
	}
}

func TestReplyTextPreferenceOrder(t *testing.T) {
	if got := replyText(&llm.Action{Args: map[string]any{"message": "msg", "summary": "sum"}, Thought: "th"}); got != "msg" {
		t.Errorf("message must win, got %q", got)
	}
	if got := replyText(&llm.Action{Args: map[string]any{"summary": "sum"}, Thought: "th"}); got != "sum" {
		t.Errorf("summary must be second, got %q", got)
	}
	if got := replyText(&llm.Action{Args: map[string]any{}, Thought: "thought text"}); got != "thought text" {
		t.Errorf("thought must be the last resort, got %q", got)
	}
}

func TestStdioREPLReadsFinalLine(t *testing.T) {
	var out strings.Builder
	sr := NewStdioREPL(strings.NewReader("hello\nfinal line"), &out)
	line, err := sr.ReadLine()
	if err != nil || line != "hello\n" {
		t.Fatalf("first line = %q %v", line, err)
	}
	line, err = sr.ReadLine()
	if err != nil || line != "final line" {
		t.Fatalf("final line without newline = %q %v", line, err)
	}
	if line, err = sr.ReadLine(); err != io.EOF {
		t.Fatalf("EOF expected, got %q %v", line, err)
	}
	sr.Printf("hello %d\n", 7)
	if !strings.Contains(out.String(), "hello 7") {
		t.Fatalf("Printf output = %q", out.String())
	}
}

// --- report.go -------------------------------------------------------------

func TestReportEmptyGoalFallsBackToTitle(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	o.cfg.Goal = ""
	r := o.Report()
	if !strings.Contains(r, "goal     worker gap test") {
		t.Fatalf("empty goal must fall back to the root title:\n%s", r)
	}
}

func TestReportShowsNodesCostsAndFailures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_report_nodes"
	cfg.Goal = "build a cli tool" // compound root
	cfg.Stream = false
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	for _, id := range []string{"impl", "integrate"} {
		if _, err := o.tree.AddChild(o.tree.RootID, id, id+" title", "desc", models.NodeTypeLeaf); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.IntegrationVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("impl", func(n *models.TaskNode) error {
		n.TokenUsage = models.TokenUsage{Calls: 2, PromptTokens: 3000, CompletionTokens: 2000, TotalTokens: 5000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.sched.UpdateNodeState("impl", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := o.sched.UpdateNodeState("integrate", models.TaskStateFailed, "integrate exploded"); err != nil {
		t.Fatal(err)
	}

	r := o.Report()
	for _, want := range []string{
		"(goal acceptance passed)",
		"nodes", "impl", "integrate title",
		"est. cost", // priced model + tokens on impl
		"failures",  // integrate failed
	} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	if !strings.Contains(r, "integrate exploded") {
		t.Errorf("failure detail missing:\n%s", r)
	}
}

func TestReportEventsUnavailableAndMalformed(t *testing.T) {
	// The event path being a directory degrades to "unavailable".
	cfg := baseTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "events", cfg.SessionID+".jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	r := o.Report()
	if !strings.Contains(r, "events   unavailable") {
		t.Fatalf("unreadable event stream must be reported:\n%s", r)
	}
	o.Close()

	// A malformed line in an otherwise readable stream is counted.
	cfg2 := baseTestConfig(t)
	o2, err := NewFromGoal(cfg2, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	o2.events.Record("tool_call", "n", map[string]any{"ok": true})
	o2.Close()
	f, err := os.OpenFile(o2.EventPath(), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this line is not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r2 := o2.Report()
	if !strings.Contains(r2, "unreadable event line(s)") {
		t.Fatalf("malformed lines must be reported:\n%s", r2)
	}
}

func TestCollectNodesAndReportOneLineEdges(t *testing.T) {
	o, _ := newWorkerOrchestrator(t, &llm.ScriptedClient{})
	if nodes := collectNodes(o.tree, nil); nodes != nil {
		t.Fatal("nil root must collect nothing")
	}
	long := strings.Repeat("word ", 20)
	if got := reportOneLine(long); len(got) != 63 || !strings.HasSuffix(got, "...") {
		t.Errorf("reportOneLine must clip long titles, got %d chars: %q", len(got), got)
	}
	if got := reportOneLine("  a\nb  "); got != "a b" {
		t.Errorf("reportOneLine must flatten newlines, got %q", got)
	}
}

// --- integration.go --------------------------------------------------------

func TestVerifyRootAcceptanceCanceled(t *testing.T) {
	dod := models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.verifyRootAcceptance(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acceptance must return ctx.Err, got %v", err)
	}
}

func TestAcceptanceFailureExhaustedRetriesFailsRoot(t *testing.T) {
	dod := models.DoD{Commands: []string{"test -f nope_missing"}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.RetryCount = 3 // MaxRetries is 3 in buildAcceptanceTree
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.handleAcceptanceFailure(context.Background(), "boom output"); err != nil {
		t.Fatal(err)
	}
	if !o.sched.HasFailed() {
		t.Fatal("exhausted acceptance retries must fail the root")
	}
	root := o.tree.GetRoot()
	if !strings.Contains(root.ErrorMsg, "after 4 attempt(s)") {
		t.Fatalf("failure message should count the attempts: %q", root.ErrorMsg)
	}
}

func TestAcceptanceFailureCanceledBackoff(t *testing.T) {
	dod := models.DoD{Commands: []string{"test -f nope_missing"}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)
	o.cfg.MaxRetries = 0 // route to the integration leaf, then back off
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.handleAcceptanceFailure(ctx, "boom output"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backoff must surface ctx.Err, got %v", err)
	}
	integrate, _ := o.tree.CloneNode("integrate")
	if integrate.State != models.TaskStatePending {
		t.Fatalf("integration leaf should be requeued before the backoff, got %s", integrate.State)
	}
}

func TestExecute_AcceptanceFailureCanceledDuringBackoff(t *testing.T) {
	// Goal acceptance fails, the failure is routed to the integration leaf,
	// and cancellation during the retry backoff surfaces as an error from the
	// execution loop.
	dod := models.DoD{Commands: []string{"test -f definitely_missing_acc"}, TimeoutSec: 10}
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_accept_exec_cancel"
	cfg.Goal = "build an http server API" // compound root
	cfg.Stream = false
	cfg.MaxRetries = 0
	cfg.RetryMinInterval = 5 * time.Second // long window: the cancel lands inside it
	cfg.RetryMaxInterval = 5 * time.Second
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	for _, id := range []string{"impl", "integrate"} {
		if _, err := o.tree.AddChild(o.tree.RootID, id, id, id, models.NodeTypeLeaf); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"main.go"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.DoD = dod
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"impl", "integrate"} {
		if err := o.sched.UpdateNodeState(id, models.TaskStateCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}

	g := NewGuider(o, newChanGuided())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel as soon as the failure has been routed to the leaf.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if n, ok := o.tree.CloneNode("integrate"); ok && n.State == models.TaskStatePending {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if err := g.execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acceptance failure must surface cancellation, got %v", err)
	}
}

func TestFindIntegrationLeafFallbacks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_find_int"
	cfg.Goal = "build a cli tool"
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	// A compound child is never the integration leaf; a leaf whose id/title
	// says "integrate" is the fallback when no output names main.go.
	if _, err := o.tree.AddChild(o.tree.RootID, "sub", "Sub", "desc", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild(o.tree.RootID, "helper", "integrate the pieces", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if got := o.findIntegrationLeaf(); got != "helper" {
		t.Fatalf("findIntegrationLeaf = %q, want helper", got)
	}

	// An explicit main.go output wins over the title fallback.
	if err := o.tree.UpdateNode("sub", func(n *models.TaskNode) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild(o.tree.RootID, "wiring", "wiring", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("wiring", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"cmd/main.go"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := o.findIntegrationLeaf(); got != "wiring" {
		t.Fatalf("main.go output must win, got %q", got)
	}
}

// --- treebroadcast.go ------------------------------------------------------

func TestTreeBroadcasterLifecycleGuards(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	ch, cancel := rec.Subscribe()
	defer cancel()

	o := &Orchestrator{events: rec, tree: engine.NewTaskTree("sess_g", "goal", "goal")}
	bc := newTreeBroadcaster(o)

	bc.close() // closing before start is a no-op
	bc.start()
	bc.start() // double start must not spawn a second loop
	bc.mark("guard")
	select {
	case ev := <-ch:
		if ev["kind"] != "tree_snapshot" {
			t.Fatalf("unexpected event: %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot after start+mark")
	}
	bc.close()
	bc.close() // double close is safe

	var nilBC *treeBroadcaster
	nilBC.mark("nope") // no panic
	nilBC.close()      // no panic

	var nilO *Orchestrator
	nilO.publishTreeSnapshot("nope") // no panic
	empty := &Orchestrator{}
	empty.publishTreeSnapshot("nope") // no events sink: no panic
}
