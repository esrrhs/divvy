package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
)

// scriptedClient returns queued content responses; it records the last
// request so tests can assert the conversation is threaded across turns.
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
	cfg.MaxSteps = 10
	repl, err := NewREPL(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	return repl
}

func TestREPL_ToolThenReply(t *testing.T) {
	// Initial goal: model calls write_file, then responds.
	client := &scriptedClient{responses: []string{
		`{"thought":"creating","action":"write_file","args":{"path":"hello.txt","content":"hi there"}}`,
		`{"thought":"done","action":"respond","args":{"message":"created the file"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.cfg.Goal = "make a file"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}

	if client.calls != 2 {
		t.Fatalf("expected 2 model calls, got %d", client.calls)
	}
	if !strings.Contains(mio.out.String(), "created the file") {
		t.Fatalf("missing reply in output:\n%s", mio.out.String())
	}
	// The tool really ran in the workspace.
	got, err := repl.sandbox.ReadFile("hello.txt")
	if err != nil || got != "hi there" {
		t.Fatalf("file not written by tool: %q %v", got, err)
	}
}

func TestREPL_MultiTurnRetainsHistory(t *testing.T) {
	// Turn 1: one tool + respond. Turn 2 (from user line): respond again.
	client := &scriptedClient{responses: []string{
		`{"action":"list_dir","args":{}}`,
		`{"action":"respond","args":{"message":"first answer"}}`,
		`{"action":"respond","args":{"message":"second answer"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.cfg.Goal = "first request"

	mio := &scriptedIO{lines: []string{"follow up request", "/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}

	out := mio.out.String()
	if !strings.Contains(out, "first answer") || !strings.Contains(out, "second answer") {
		t.Fatalf("both turns should reply:\n%s", out)
	}
	if client.calls != 3 {
		t.Fatalf("expected 3 calls across 2 turns, got %d", client.calls)
	}
	// The final request must carry the earlier conversation (system + first
	// turn user/tool/assistant exchange + second turn user), proving history
	// is retained rather than reset between turns.
	if len(client.lastMsgs) < 5 {
		t.Fatalf("expected threaded history in final request, got %d messages", len(client.lastMsgs))
	}
}

func TestREPL_InvalidActionGetsCorrected(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`not a json action`,
		`{"action":"respond","args":{"message":"recovered"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.cfg.Goal = "do something"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mio.out.String(), "recovered") {
		t.Fatalf("model should recover after invalid action:\n%s", mio.out.String())
	}
}

func TestREPL_SlashCommands(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"action":"respond","args":{"message":"ok"}}`,
	}}
	repl := newTestREPL(t, client)

	mio := &scriptedIO{lines: []string{"/help", "/clear", "/status", "/bogus", "/quit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	out := mio.out.String()
	if !strings.Contains(out, "Commands:") {
		t.Fatal(" /help did not print")
	}
	if !strings.Contains(out, "conversation cleared") {
		t.Fatal("/clear did not print")
	}
	if !strings.Contains(out, "workspace:") {
		t.Fatal("/status did not print")
	}
	if !strings.Contains(out, "unknown command") {
		t.Fatal("unknown slash command not handled")
	}
}

func TestREPL_EOFEndsSession(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"action":"respond","args":{"message":"ok"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.cfg.Goal = "initial"

	// No queued lines → immediate EOF after the initial turn.
	if err := repl.Run(context.Background(), &scriptedIO{}); err != nil {
		t.Fatal(err)
	}
}

func TestREPL_ToolErrorFedBack(t *testing.T) {
	// A tool call with invalid args produces an error that is fed back; the
	// model then responds. The session stays alive.
	client := &scriptedClient{responses: []string{
		`{"action":"read_file","args":{}}`,
		`{"action":"respond","args":{"message":"noted"}}`,
	}}
	repl := newTestREPL(t, client)
	repl.cfg.Goal = "read a missing file"

	mio := &scriptedIO{lines: []string{"/exit"}}
	if err := repl.Run(context.Background(), mio); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("error should be fed back for a second call, got %d", client.calls)
	}
}
