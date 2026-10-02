package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
)

// scriptedClient returns queued content responses; it records the last
// request so tests can assert what the foreman carried.
type scriptedClient struct {
	responses []string
	calls     int
	lastMsgs  []llm.Message
}

func (c *scriptedClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	idx := c.calls
	if idx >= len(c.responses) {
		idx = len(c.responses) - 1
	}
	content := c.responses[idx]
	c.calls++
	c.lastMsgs = req.Messages
	return &llm.Response{
		Content: content,
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5},
	}, nil
}

// scriptedIO feeds queued user lines and captures all printed output.
type scriptedIO struct {
	lines []string
	idx   int
	out   strings.Builder
}

func (s *scriptedIO) ReadLine() (string, error) {
	if s.idx >= len(s.lines) {
		return "", io.EOF
	}
	v := s.lines[s.idx]
	s.idx++
	return v, nil
}

func (s *scriptedIO) Printf(format string, a ...any) {
	fmt.Fprintf(&s.out, format, a...)
}

func newTestREPL(t *testing.T, client llm.Client) *REPL {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.MaxSteps = 10
	repl, err := NewREPL(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	return repl
}

func TestREPL_DispatchLeafThenReply(t *testing.T) {
	// Foreman dispatches one leaf; the leaf writes a real file in its own
	// worker context and finishes; the foreman then reports to the user.
	client := &scriptedClient{responses: []string{
		`{"thought":"need a file","action":"dispatch","args":{"tasks":[{"title":"create hello","description":"Create file hello.txt containing exactly: hi there"}]}}`,
		`{"thought":"write","action":"write_file","args":{"path":"hello.txt","content":"hi there"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"created hello.txt"}}`,
		`{"thought":"report","action":"respond","args":{"message":"created the file"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "make a file"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}

	if client.calls != 4 {
		t.Fatalf("expected 4 calls (dispatch, leaf x2, respond), got %d", client.calls)
	}
	if repl.leaves != 1 {
		t.Fatalf("expected exactly 1 leaf run, got %d", repl.leaves)
	}
	if !strings.Contains(mio.out.String(), "created the file") {
		t.Fatalf("missing foreman reply:\n%s", mio.out.String())
	}
	// The leaf really did the work in the workspace.
	got, err := repl.o.sandbox.ReadFile("hello.txt")
	if err != nil || got != "hi there" {
		t.Fatalf("file not written by leaf: %q %v", got, err)
	}
}

func TestREPL_MultiTurnCarriesCompressedSummary(t *testing.T) {
	// Turn 1: dispatch → leaf finish → respond. Turn 2: the foreman answers
	// directly. The turn-2 request must contain the compressed work log of
	// turn 1 (including its reply marker) but none of the raw turn-1 JSON.
	client := &scriptedClient{responses: []string{
		`{"thought":"plan","action":"dispatch","args":{"tasks":[{"title":"do first","description":"Do the first piece of work"}]}}`,
		`{"thought":"done","action":"finish","args":{"summary":"first-leaf-result"}}`,
		`{"thought":"report","action":"respond","args":{"message":"first-done-marker"}}`,
		`{"thought":"answer","action":"respond","args":{"message":"second reply marker"}}`,
	}}
	repl := newTestREPL(t, client)

	mio := &scriptedIO{lines: []string{"first request", "second request", "/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if client.calls != 4 {
		t.Fatalf("expected 4 calls, got %d", client.calls)
	}

	// Final request: system + work-log user message + current user message.
	if len(client.lastMsgs) != 3 {
		t.Fatalf("turn 2 should carry system + work log + current msg, got %d messages: %+v", len(client.lastMsgs), client.lastMsgs)
	}
	blob := ""
	for _, m := range client.lastMsgs {
		blob += m.Content
	}
	if !strings.Contains(blob, "first-done-marker") || !strings.Contains(blob, "first-leaf-result") {
		t.Fatalf("work log missing turn-1 summary:\n%s", blob)
	}
	if strings.Contains(blob, "Do the first piece of work") {
		t.Fatal("raw turn-1 leaf instruction must not be carried; only its compressed summary")
	}
	if len(repl.summaries) != 2 {
		t.Fatalf("expected 2 compressed turns kept (one per turn), got %d", len(repl.summaries))
	}
}

func TestREPL_InvalidActionGetsCorrected(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`not a json action`,
		`{"thought":"ok","action":"respond","args":{"message":"recovered"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "do something"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mio.out.String(), "recovered") {
		t.Fatalf("foreman should recover after invalid action:\n%s", mio.out.String())
	}
}

func TestREPL_ForemanRespondsWithoutLeaf(t *testing.T) {
	// A direct answer dispatches no leaves; the reply is still shown.
	client := &scriptedClient{responses: []string{
		`{"thought":"just answer","action":"respond","args":{"message":"direct answer"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Goal = "hello"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if repl.leaves != 0 {
		t.Fatalf("no leaf should run, got %d", repl.leaves)
	}
	if !strings.Contains(mio.out.String(), "direct answer") {
		t.Fatal("missing direct answer")
	}
}

func TestREPL_SlashCommands(t *testing.T) {
	client := &scriptedClient{responses: []string{""}}
	repl := newTestREPL(t, client)
	repl.summaries = []string{"an earlier turn"}

	mio := &scriptedIO{lines: []string{"/help", "/clear", "/status", "/bogus", "/quit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	out := mio.out.String()
	for _, want := range []string{"Architecture", "work log cleared", "leaf runs:", "unknown command"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if len(repl.summaries) != 0 {
		t.Fatal("/clear should drop the work log")
	}
}

func TestREPL_EOFEndsSession(t *testing.T) {
	client := &scriptedClient{responses: []string{""}}
	repl := newTestREPL(t, client)
	// No goal, no queued lines → immediate EOF.
	if err := repl.Run(context.Background(), &scriptedIO{}); err != nil {
		t.Fatal(err)
	}
}

func TestParseForemanTasks(t *testing.T) {
	a := &llm.Action{Args: map[string]any{
		"tasks": []any{
			map[string]any{"title": "A", "description": "do A"},
			map[string]any{"description": "some long unnamed task"},
			map[string]any{"title": "", "description": "   "},
			"not-an-object",
		},
	}}
	tasks := parseForemanTasks(a)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 valid tasks, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].title != "A" || tasks[1].title != "some long unnamed task" {
		t.Fatalf("unexpected titles: %+v", tasks)
	}
}

func TestREPL_ForemanTrackedWithLeaf(t *testing.T) {
	// After a dispatch turn the single tracker carries both the foreman call
	// and the leaf worker call, under distinct kinds.
	client := &scriptedClient{responses: []string{
		`{"thought":"plan","action":"dispatch","args":{"tasks":[{"title":"do","description":"Do the thing"}]}}`,
		`{"thought":"done","action":"finish","args":{"summary":"did it"}}`,
		`{"thought":"report","action":"respond","args":{"message":"ok"}}`,
	}}
	repl := newTestREPL(t, client) // default model gpt-4o-mini is priced
	repl.o.cfg.Goal = "go"
	if err := repl.turn(context.Background(), "go", &scriptedIO{}); err != nil {
		t.Fatal(err)
	}

	kinds := repl.o.usage.ByKind()
	fm, hasFm := kinds[usageKindForeman]
	_, hasWr := kinds["worker"]
	if !hasFm || !hasWr {
		t.Fatalf("expected foreman + worker kinds, got %+v", kinds)
	}
	// Two foreman calls (dispatch + respond) = 20/10; one leaf call = 10/5.
	if fm.PromptTokens != 20 || fm.CompletionTokens != 10 {
		t.Fatalf("unexpected foreman usage %+v", fm)
	}
	total, calls := repl.o.usage.Total()
	if calls != 3 {
		t.Fatalf("expected 3 tracked calls (2 foreman + 1 leaf), got %d", calls)
	}
	if total.PromptTokens != 30 || total.CompletionTokens != 15 {
		t.Fatalf("unexpected combined usage %+v", total)
	}
}

func TestREPL_StatusShowsCost(t *testing.T) {
	// Priced model (gpt-4o-mini): /status prints per-kind lines and a total
	// with an est. dollar figure.
	client := &scriptedClient{responses: []string{
		`{"thought":"plan","action":"dispatch","args":{"tasks":[{"title":"do","description":"Do the thing"}]}}`,
		`{"thought":"done","action":"finish","args":{"summary":"did it"}}`,
		`{"thought":"report","action":"respond","args":{"message":"ok"}}`,
	}}
	repl := newTestREPL(t, client)
	if err := repl.turn(context.Background(), "go", &scriptedIO{}); err != nil {
		t.Fatal(err)
	}
	mio := &scriptedIO{}
	repl.slash(context.Background(), "/status", mio)
	out := mio.out.String()

	if !strings.Contains(out, "foreman:") || !strings.Contains(out, "worker:") {
		t.Fatalf("per-kind lines missing:\n%s", out)
	}
	if !strings.Contains(out, "total:") || !strings.Contains(out, "est. $") {
		t.Fatalf("total with estimated cost missing:\n%s", out)
	}
	// 2 foreman calls (20/10) + 1 leaf call (10/5):
	// 30 prompt * 0.15/1M + 15 completion * 0.60/1M = $0.000013
	if !strings.Contains(out, "$0.000013") {
		t.Fatalf("unexpected cost figure:\n%s", out)
	}
}

func TestREPL_StatusNoPriceHint(t *testing.T) {
	// A model absent from the price table: /status still shows tokens, but
	// replaces cost with the -pricing hint instead of a fake number.
	client := &scriptedClient{responses: []string{
		`{"thought":"answer","action":"respond","args":{"message":"hi"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.o.cfg.Model = "qwen3.8-local:27b"
	if err := repl.turn(context.Background(), "hello", &scriptedIO{}); err != nil {
		t.Fatal(err)
	}
	mio := &scriptedIO{}
	repl.slash(context.Background(), "/status", mio)
	out := mio.out.String()

	if strings.Contains(out, "est. $") {
		t.Fatal("must not invent a cost for an unpriced model")
	}
	if !strings.Contains(out, "no price for this model") || !strings.Contains(out, "-pricing") {
		t.Fatalf("expected the -pricing hint:\n%s", out)
	}
}
