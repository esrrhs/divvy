package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/engine"
	"github.com/esrrhs/go_llm_engine/pkg/llm"
	"github.com/esrrhs/go_llm_engine/pkg/models"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
)

// REPLIO is the REPL's input/output boundary. Abstracting it keeps the REPL
// testable without touching a real terminal.
type REPLIO interface {
	ReadLine() (string, error)
	Printf(format string, a ...any)
}

const (
	// replMaxSummaries bounds how many compressed earlier turns are carried.
	replMaxSummaries = 8
	// replMaxDispatch bounds how many dispatch rounds one user turn may take.
	replMaxDispatch = 10

	// usageKindForeman is the usage-tracker kind for outer foreman calls.
	usageKindForeman = "foreman"
)

// REPL is the interactive session built as a foreman plus independent leaf
// workers: a lightweight outer conversation understands the user and decides
// what work to dispatch; every actual file/shell action happens inside a leaf
// with its own fresh context (runWorker), and the outer layer only ever sees
// the leaf's short summary. Earlier turns survive only as compressed
// summaries, never as raw conversation.
type REPL struct {
	o         *Orchestrator
	summaries []string
	leaves    int
}

// NewREPL builds an interactive session operating directly in cfg.WorkDir
// (no mirror isolation — it is the user's live workspace).
func NewREPL(cfg Config, client llm.Client) (*REPL, error) {
	tree := engine.NewTaskTree("interactive", "interactive session", "interactive session")
	o, err := New(cfg, tree, client, NewLogger(cfg.Verbose))
	if err != nil {
		return nil, err
	}
	return &REPL{o: o}, nil
}

// Run drives the prompt → foreman/leaf loop until /exit, EOF, or cancellation.
// If cfg.Goal is set it is executed as the first turn automatically.
func (r *REPL) Run(ctx context.Context, term REPLIO) (runErr error) {
	runStart := time.Now()
	r.o.recordSessionStart("interactive")
	defer func() { r.o.recordSessionEnd("interactive", runStart, runErr) }()
	defer r.o.Close()

	term.Printf("go_llm_engine interactive — model %s, workspace %s\n", r.o.cfg.Model, r.o.sandbox.Root)
	term.Printf("foreman + independent leaf workers; type /help for commands\n")

	if strings.TrimSpace(r.o.cfg.Goal) != "" {
		if err := r.turn(ctx, r.o.cfg.Goal, term); err != nil {
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
		r.summaries = nil
		term.Printf("work log cleared (files untouched)\n")
	case "/help":
		term.Printf(replHelp)
	case "/status":
		total, calls := r.o.usage.Total()
		term.Printf("model:      %s\nworkspace:  %s\ncalls:      %d    leaf runs: %d    work log: %d turn(s) (max %d)\n",
			r.o.cfg.Model, r.o.sandbox.Root, calls, r.leaves, len(r.summaries), replMaxSummaries)
		keys, kinds := r.o.usage.SortedKinds()
		for _, k := range keys {
			term.Printf("  %-9s %s\n", k+":", r.o.formatKindUsage(kinds[k]))
		}
		term.Printf("total:      %d prompt + %d completion = %d tokens",
			total.PromptTokens, total.CompletionTokens, total.TotalTokens)
		if c, ok := r.o.costLabel(total); ok {
			term.Printf(", est. %s\n", c)
		} else {
			term.Printf("\ncost:       no price for this model — pass -pricing '{\"%s\":{\"input\":..,\"output\":..}}'\n", r.o.cfg.Model)
		}
	default:
		term.Printf("unknown command %q — try /help\n", cmd)
	}
	return false
}

// foremanTask is one self-contained unit of work handed to a leaf.
type foremanTask struct {
	title       string
	description string
}

// turn runs one user request through the foreman. The foreman either responds
// directly (clarify/summarize/chat) or dispatches one or more independent
// leaf workers; their summaries are fed back, and this repeats until the
// foreman responds. Only a compressed summary of the turn survives.
func (r *REPL) turn(ctx context.Context, userText string, term REPLIO) error {
	conv := []llm.Message{{Role: llm.RoleUser, Content: userText}}

	var leafResults []string
	for step := 1; step <= replMaxDispatch; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		raw, err := r.outerChat(ctx, conv)
		if err != nil {
			return err
		}

		act, perr := llm.ParseAction(raw)
		if perr != nil {
			conv = append(conv,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: "That was not a valid action. Reply with exactly one JSON object."},
			)
			continue
		}

		if act.Name == "respond" || act.Name == "finish" {
			text := replyText(act)
			if strings.TrimSpace(text) != "" {
				term.Printf("%s\n", text)
			}
			r.recordTurn(userText, leafResults, text)
			return nil
		}

		if act.Name != "dispatch" {
			conv = append(conv,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: `You have no file or shell tools of your own. Use {"action":"dispatch","args":{"tasks":[...]}} to do work, or {"action":"respond",...} to talk.`},
			)
			continue
		}

		tasks := parseForemanTasks(act)
		if len(tasks) == 0 {
			conv = append(conv,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: "dispatch needs a non-empty \"tasks\" array; each task needs title and description."},
			)
			continue
		}

		conv = append(conv, llm.Message{Role: llm.RoleAssistant, Content: raw})

		for _, t := range tasks {
			term.Printf("  -> leaf %d: %s\n", r.leaves+1, t.title)
			node := r.newLeafNode(t)
			summary, werr := r.o.runWorker(ctx, r.o.sandbox, node, "")
			r.leaves++
			result := summary
			if werr != nil {
				result = "ERROR: " + werr.Error()
			}
			leafResults = append(leafResults, t.title+": "+result)
		}

		conv = append(conv, llm.Message{
			Role:    llm.RoleUser,
			Content: "Leaf result(s):\n" + strings.Join(leafResults, "\n"),
		})
	}

	term.Printf("[stopped after %d dispatch rounds; send another message or adjust the request]\n", replMaxDispatch)
	return nil
}

// newLeafNode builds the synthetic leaf a dispatched task runs in. The node is
// self-contained; the compressed work log is appended so workers retain
// continuity with earlier turns despite their fresh context.
func (r *REPL) newLeafNode(t foremanTask) *models.TaskNode {
	desc := strings.TrimSpace(t.description)
	if len(r.summaries) > 0 {
		desc += "\n\nRecent work log (compressed summaries — use these for context):\n" +
			strings.Join(r.summaries, "\n")
	}
	return models.NewTaskNode(
		fmt.Sprintf("leaf_%d", r.leaves+1), "",
		strings.TrimSpace(t.title), desc, models.NodeTypeLeaf, 0)
}

// outerChat performs one foreman model call with system prompt, compressed
// work log, and the ephemeral conversation for this turn only.
func (r *REPL) outerChat(ctx context.Context, conv []llm.Message) (string, error) {
	msgs := make([]llm.Message, 0, len(conv)+2)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: r.foremanSystem()})
	if len(r.summaries) > 0 {
		msgs = append(msgs, llm.Message{
			Role:    llm.RoleUser,
			Content: "Earlier work log (compressed summaries, not raw conversation):\n" + strings.Join(r.summaries, "\n"),
		})
	}
	msgs = append(msgs, conv...)

	start := time.Now()
	resp, err := r.o.llm.Chat(ctx, llm.Request{
		Model:       r.o.cfg.Model,
		Messages:    msgs,
		Temperature: r.o.cfg.Temperature,
		MaxTokens:   r.o.cfg.MaxTokens,
	})
	r.o.events.LLMCall(usageKindForeman, "", llm.Request{
		Model:       r.o.cfg.Model,
		Messages:    msgs,
		Temperature: r.o.cfg.Temperature,
		MaxTokens:   r.o.cfg.MaxTokens,
	}, resp, start, err)
	if err != nil {
		return "", fmt.Errorf("foreman request failed: %w", err)
	}
	// Foreman calls go through the same tracker as leaf workers, so every
	// call in the session is visible and priced in one place.
	r.o.usage.Add(usageKindForeman, resp.Usage)
	return resp.Content, nil
}

// recordTurn compresses the finished turn into a short structured summary and
// keeps only the most recent replMaxSummaries entries.
func (r *REPL) recordTurn(userText string, leafResults []string, reply string) {
	var b strings.Builder
	fmt.Fprintf(&b, "you: %s", truncate(strings.TrimSpace(userText), 120))
	for _, lr := range leafResults {
		fmt.Fprintf(&b, "\n  leaf: %s", truncate(lr, 200))
	}
	fmt.Fprintf(&b, "\n  reply: %s", truncate(strings.TrimSpace(reply), 160))

	r.summaries = append(r.summaries, b.String())
	if len(r.summaries) > replMaxSummaries {
		r.summaries = r.summaries[len(r.summaries)-replMaxSummaries:]
	}
}

func parseForemanTasks(a *llm.Action) []foremanTask {
	raw, ok := a.Args["tasks"].([]any)
	if !ok {
		return nil
	}
	out := make([]foremanTask, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title, _ := stringFromArgs(m, "title")
		desc, _ := stringFromArgs(m, "description")
		title = strings.TrimSpace(title)
		desc = strings.TrimSpace(desc)
		if title == "" && desc == "" {
			continue
		}
		if title == "" {
			title = firstLine(desc)
		}
		out = append(out, foremanTask{title: title, description: desc})
	}
	return out
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

func (r *REPL) foremanSystem() string {
	return fmt.Sprintf(`You are the foreman of an interactive coding session in the user's live workspace (%s; detected toolchain: %s).
You do NOT have file or shell tools yourself. You plan and delegate; independent leaf workers do all real work.

Output EXACTLY ONE JSON object per reply:
1. To do work, dispatch one or more self-contained tasks:
{"thought":"short plan","action":"dispatch","args":{"tasks":[{"title":"short title","description":"exact, self-contained instructions including file paths, the detected stack, and expected result"}]}}
You will then receive each leaf's short result; dispatch more if needed, or answer.
2. To talk to the user — answering, asking a clarifying question, or summarizing:
{"thought":"...","action":"respond","args":{"message":"your message"}}

Rules:
- Leaf workers have a FRESH context: they do not see this conversation. Every task description must be self-contained — resolve references like "that file" into concrete paths and requirements, using the workspace and the earlier work log.
- Prefer one focused task per leaf; do not bundle unrelated changes.
- Use respond to ask when genuinely ambiguous; never claim work that a leaf did not report as done.
- The earlier work log is compressed summaries only; trust the leaf results over assumptions.`,
		r.o.sandbox.Root, tools.DetectProject(r.o.sandbox.Root))
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

const replHelp = `Architecture: a foreman (this conversation) dispatches independent leaf
workers; each leaf runs in its own fresh context and only its summary
comes back. Earlier turns are kept as compressed summaries, not raw chat.

Commands:
  /help    show this help
  /clear   reset the earlier-turn work log (keeps your files)
  /status  model, workspace, leaf runs, token usage, work log
  /exit    quit (also /quit, or Ctrl-D)

Anything else is sent to the foreman, which dispatches leaves that work in
this workspace with: list_dir, read_file, write_file, replace_lines,
run_bash, search_files. Describe concrete, self-contained requests; ask
follow-ups on the next line and the foreman will use the work log.
Note: a command's background processes are stopped when the command returns,
so run long-lived servers in a separate terminal.
`
