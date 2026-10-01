package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
)

// REPLIO is the REPL's input/output boundary. Abstracting it keeps the REPL
// testable without touching a real terminal.
type REPLIO interface {
	ReadLine() (string, error)
	Printf(format string, a ...any)
}

// REPL is the interactive coding session. Unlike the batch orchestrator it
// keeps a live conversation across turns; the model drives the same workspace
// tools and returns control to the user after each reply.
type REPL struct {
	cfg      Config
	client   llm.Client
	sandbox  *tools.Sandbox
	system   string
	msgs     []llm.Message
	prompt   int
	complete int
}

// NewREPL builds an interactive session operating directly in cfg.WorkDir
// (no mirror isolation — it is the user's live workspace).
func NewREPL(cfg Config, client llm.Client) (*REPL, error) {
	sb, err := tools.NewSandbox(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	return &REPL{
		cfg:     cfg,
		client:  client,
		sandbox: sb,
		system:  replSystemPrompt(),
	}, nil
}

// Run drives the prompt → agent-turn loop until /exit, EOF, or cancellation.
// If cfg.Goal is set it is executed as the first turn automatically.
func (r *REPL) Run(ctx context.Context, term REPLIO) error {
	term.Printf("go_llm_engine interactive REPL — model %s, workspace %s\n", r.cfg.Model, r.sandbox.Root)
	term.Printf("type /help for commands, /exit to quit\n")

	if strings.TrimSpace(r.cfg.Goal) != "" {
		if err := r.turn(ctx, r.cfg.Goal, term); err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		term.Printf("\n> ")
		line, err := term.ReadLine()
		if err == io.EOF {
			term.Printf("\n")
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if stop := r.slash(ctx, line, term); stop {
				return nil
			}
			continue
		}

		if err := r.turn(ctx, line, term); err != nil {
			if ctx.Err() != nil {
				return err
			}
			// A transient model error must not kill the session.
			term.Printf("error: %v\n", err)
		}
	}
}

// slash handles a command. It returns true when the REPL should terminate.
func (r *REPL) slash(ctx context.Context, cmd string, term REPLIO) bool {
	switch cmd {
	case "/exit", "/quit":
		term.Printf("bye\n")
		return true
	case "/clear":
		r.msgs = nil
		term.Printf("conversation cleared (workspace untouched)\n")
	case "/help":
		term.Printf(replHelp)
	case "/status":
		term.Printf("model:      %s\nworkspace:  %s\nmessages:   %d\ntokens:     %d prompt + %d completion\n",
			r.cfg.Model, r.sandbox.Root, len(r.msgs), r.prompt, r.complete)
	default:
		term.Printf("unknown command %q — try /help\n", cmd)
	}
	return false
}

// turn runs one user request through the agent: repeatedly call the model,
// execute tool actions, feed results back, until the model emits a "respond"
// (its reply to the user) or the per-turn step limit is reached.
func (r *REPL) turn(ctx context.Context, userText string, term REPLIO) error {
	r.msgs = append(r.msgs, llm.Message{Role: llm.RoleUser, Content: userText})

	maxSteps := r.cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}

	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		msgs := make([]llm.Message, 0, len(r.msgs)+1)
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: r.system})
		msgs = append(msgs, r.msgs...)

		resp, err := r.client.Chat(ctx, llm.Request{
			Model:       r.cfg.Model,
			Messages:    msgs,
			Temperature: r.cfg.Temperature,
			MaxTokens:   r.cfg.MaxTokens,
		})
		if err != nil {
			return fmt.Errorf("model request failed: %w", err)
		}
		r.addUsage(resp.Usage)

		raw := strings.TrimSpace(resp.Content)
		action, perr := llm.ParseAction(raw)
		if perr != nil {
			// Feed a correction hint and try again within the step budget.
			r.msgs = append(r.msgs,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: "That was not a valid action. Reply with exactly one JSON object: {\"thought\":...,\"action\":...,\"args\":{...}}"},
			)
			continue
		}

		// End of turn: reply to the user. "finish" is the worker spelling;
		// accept it defensively as a reply.
		if action.Name == "respond" || action.Name == "finish" {
			text := replyText(action)
			r.msgs = append(r.msgs, llm.Message{Role: llm.RoleAssistant, Content: raw})
			if strings.TrimSpace(text) != "" {
				term.Printf("%s\n", text)
			}
			return nil
		}

		// Otherwise it is a workspace tool: show activity, run it, feed the
		// result back so the model can continue.
		term.Printf("  -> %s\n", replDescribe(action))
		out, callErr := r.sandbox.Call(ctx, action.Name, action.Args)
		result := out
		if callErr != nil {
			result = "ERROR: " + callErr.Error()
		}
		r.msgs = append(r.msgs,
			llm.Message{Role: llm.RoleAssistant, Content: raw},
			llm.Message{Role: llm.RoleUser, Content: "Tool result:\n" + result},
		)
	}

	term.Printf("[stopped after %d tool steps; send another message or adjust the request]\n", maxSteps)
	return nil
}

func (r *REPL) addUsage(u llm.Usage) {
	r.prompt += u.PromptTokens
	r.complete += u.CompletionTokens
}

func replyText(a *llm.Action) string {
	if s, ok := a.Args["message"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := a.Args["summary"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return a.Thought
}

func replDescribe(a *llm.Action) string {
	get := func(k string) string {
		if s, ok := a.Args[k].(string); ok {
			return s
		}
		return ""
	}
	switch a.Name {
	case "list_dir", "read_file", "write_file", "replace_lines":
		return a.Name + " " + get("path")
	case "run_bash":
		return a.Name + " " + truncate(get("command"), 60)
	case "search_files":
		return a.Name + " " + truncate(get("pattern"), 60)
	default:
		return a.Name
	}
}

// StdioREPL connects REPLIO to a terminal.
type StdioREPL struct {
	in  *bufio.Reader
	out io.Writer
}

func NewStdioREPL(in io.Reader, out io.Writer) *StdioREPL {
	return &StdioREPL{in: bufio.NewReader(in), out: out}
}

func (s *StdioREPL) ReadLine() (string, error) {
	line, err := s.in.ReadString('\n')
	if err == io.EOF && strings.TrimSpace(line) != "" {
		return line, nil // final line without a trailing newline
	}
	return line, err
}

func (s *StdioREPL) Printf(format string, a ...any) {
	fmt.Fprintf(s.out, format, a...)
}

func replSystemPrompt() string {
	return `You are an interactive coding agent operating directly inside the user's workspace. ` +
		`Do real work with the tools below; never claim changes you did not make.

To act, output EXACTLY ONE JSON object and nothing else:
{"thought":"short reasoning","action":"<tool>","args":{ ... }}

Tools:
` + tools.Descriptions() + `

To talk to the user — answering, asking a clarifying question, or summarizing finished work — output:
{"thought":"...","action":"respond","args":{"message":"your message"}}

Rules:
- Only one JSON object per response.
- Use the tools to inspect and change the workspace, then call respond when done.
- Keep replies concise and concrete (what changed, how to verify).
- If a request is ambiguous or you lack information, call respond and ask rather than guessing.
- Do not use "finish"; in this mode you end a turn with "respond".`
}

const replHelp = `Commands:
  /help    show this help
  /clear   reset the conversation (keeps your files)
  /status  model, workspace, message count and token usage
  /exit    quit (also /quit, or Ctrl-D)

Anything else is sent to the agent, which works in this workspace with:
  list_dir, read_file, write_file, replace_lines, run_bash, search_files
The agent calls tools to do the work and then replies; ask it to clarify,
iterate, or add more on the next line — the conversation is retained.
Note: a command's background processes are stopped when the command returns,
so run long-lived servers in a separate terminal.
`
