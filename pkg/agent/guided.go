package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
)

// GuidedIO is the input/output boundary for the human-in-the-loop flow.
// Lines delivers user input as it is typed; the implementation is
// channel-based so the controller can select on it alongside a running
// leaf and a worker question.
type GuidedIO interface {
	Lines() <-chan string
	Printf(format string, a ...any)
}

// stdioGuided connects GuidedIO to a terminal. A goroutine scans lines and
// forwards them; EOF closes the channel.
type stdioGuided struct {
	ch  chan string
	out io.Writer
}

func NewStdioGuided(in io.Reader, out io.Writer) *stdioGuided {
	g := &stdioGuided{ch: make(chan string), out: out}
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			g.ch <- sc.Text()
		}
		close(g.ch)
	}()
	return g
}

func (g *stdioGuided) Lines() <-chan string { return g.ch }
func (g *stdioGuided) Printf(format string, a ...any) {
	fmt.Fprintf(g.out, format, a...)
}

// ErrPaused indicates the user paused execution; the tree has been saved.
var ErrPaused = fmt.Errorf("execution paused")

// errStdinClosed means the input stream ended; the guided run should terminate
// rather than loop. It is handled internally and never surfaced.
var errStdinClosed = fmt.Errorf("stdin closed")

// Guider runs plan → review → execute with human approval, mid-run plan
// changes, and worker questions.
type Guider struct {
	o  *Orchestrator
	io GuidedIO

	// Handshake channels for a worker's "ask": the leaf goroutine sends the
	// question on askCh and blocks on answerCh; the controller (single
	// consumer of input) relays the reply.
	askCh    chan string
	answerCh chan string
}

// NewGuider wraps an orchestrator that has a goal but has not yet run.
func NewGuider(o *Orchestrator, io GuidedIO) *Guider {
	return &Guider{
		o:        o,
		io:       io,
		askCh:    make(chan string),
		answerCh: make(chan string),
	}
}

// Run executes the full guided lifecycle.
func (g *Guider) Run(ctx context.Context) (runErr error) {
	runStart := time.Now()
	g.o.recordSessionStart("guided")
	defer func() { g.o.recordSessionEnd("guided", runStart, runErr) }()

	// Budget guardrails (-max-cost / -budget-tokens) must apply here too;
	// without this they silently did nothing in guided runs.
	ctx = g.o.startBudget(ctx)

	g.o.log.Banner("divvy (guided)")
	g.io.Printf("session %s\nmodel   %s\nworkdir %s\ngoal    %s\n",
		g.o.tree.ID, g.o.cfg.Model, g.o.sandbox.Root, g.o.cfg.Goal)

	// On resume, if execution already started (some leaves completed), skip
	// planning and review and continue straight into execution.
	done, _, _ := g.o.tree.GetLeafProgress()
	resuming := done > 0

	if !resuming {
		// ① PLANNING
		g.io.Printf("planning...\n")
		if err := g.o.buildPlan(ctx); err != nil {
			return err
		}
		g.o.printTree()

		// ② REVIEW
		if err := g.review(ctx); err != nil {
			if err == errStdinClosed {
				return nil
			}
			return err
		}
	}

	// ③ EXECUTION
	g.o.askHook = func(question string) string {
		g.askCh <- question
		return <-g.answerCh
	}
	g.io.Printf("plan approved — executing\n")
	if err := g.execute(ctx); err != nil {
		if err == errStdinClosed {
			return nil
		}
		return err
	}
	return nil
}

// review shows the plan and waits for approval or adjustments.
func (g *Guider) review(ctx context.Context) error {
	for {
		g.io.Printf("\nplan ready — /approve to start, type adjustments, or /abort\n> ")
		select {
		case line, ok := <-g.io.Lines():
			if !ok {
				// EOF means the user went away; it must NOT approve the plan.
				// Save it unexecuted so nothing starts unattended.
				_ = g.o.checkpoint()
				g.io.Printf("input closed — plan saved (not executed); review again with -guided -resume\n")
				return errStdinClosed
			}
			switch {
			case isApprove(line):
				g.o.events.Record("plan_review", "", map[string]any{"decision": "approved"})
				return nil
			case isAbort(line):
				g.io.Printf("aborted; plan saved (not executed)\n")
				g.o.events.Record("plan_review", "", map[string]any{"decision": "aborted"})
				_ = g.o.checkpoint()
				return fmt.Errorf("plan aborted by user")
			default:
				g.io.Printf("revising plan: %s\n", strings.TrimSpace(line))
				g.o.events.Record("plan_review", "", map[string]any{
					"decision": "adjusted",
					"feedback": clip(strings.TrimSpace(line), evReasonChars),
				})
				if err := g.o.replan(ctx, line); err != nil {
					return err
				}
				g.o.printTree()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// execute runs ready leaves serially, with goal-level acceptance at the
// end, decomposition of nodes that appear mid-run, and interjection.
func (g *Guider) execute(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			_ = g.o.checkpoint()
			return err
		}

		// A node may need (re)decomposition, e.g. a failed leaf re-split
		// into a compound during execution.
		if dn := g.o.sched.GetNextDecomposableNode(); dn != nil {
			g.io.Printf("decomposing %s\n", dn.ID)
			if err := g.o.decompose(ctx, dn); err != nil {
				return err
			}
			g.o.printTree()
			continue
		}

		// Goal-level acceptance once every child is done.
		if g.o.sched.RootNeedsAcceptance() {
			if err := g.o.verifyRootAcceptance(ctx); err != nil {
				return err
			}
		}
		if g.o.sched.IsComplete() {
			g.o.printTree()
			g.o.logUsage()
			g.io.Printf("all done.\n")
			return nil
		}

		ready := g.o.sched.GetReadyLeafNodes()
		if len(ready) == 0 {
			// Nothing runnable and not complete: ask the user how to proceed.
			g.io.Printf("no leaf is ready — type /add <instruction>, /pause, or other guidance\n> ")
			select {
			case line, ok := <-g.io.Lines():
				if !ok {
					_ = g.o.checkpoint()
					g.io.Printf("input closed — progress saved; continue with -guided -resume\n")
					return errStdinClosed
				}
				if err := g.handleExecLine(ctx, line); err != nil {
					if err == ErrPaused {
						return err
					}
					g.io.Printf("%v\n", err)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}

		if err := g.runLeafInteractive(ctx, ready[0]); err != nil {
			if err == ErrPaused {
				return err
			}
			if err == errStdinClosed {
				_ = g.o.checkpoint()
				g.io.Printf("input closed — progress saved; continue with -guided -resume\n")
				return errStdinClosed
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// runLeafInteractive executes one leaf in a goroutine while selecting on
// user input (pause / queued plan changes) and worker questions.
func (g *Guider) runLeafInteractive(ctx context.Context, leaf *models.TaskNode) error {
	leafCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- g.o.executeLeaf(leafCtx, leaf)
	}()

	var queued []string
	for {
		select {
		case err := <-doneCh:
			cancel()
			for _, q := range queued {
				if e := g.handleExecLine(ctx, q); e != nil {
					g.io.Printf("%v\n", e)
				}
			}
			return err

		case line, ok := <-g.io.Lines():
			if !ok {
				g.unblockAsker("input closed")
				cancel()
				<-doneCh
				return errStdinClosed
			}
			if isPause(line) {
				g.unblockAsker("execution paused by user")
				cancel()
				<-doneCh
				_ = g.o.checkpoint()
				g.o.events.Record("user_pause", "", map[string]any{"where": "leaf_running"})
				g.io.Printf("paused and saved.\n")
				return ErrPaused
			}
			if strings.TrimSpace(line) == "/plan" {
				// View-only; print the current tree right away, do not queue.
				g.o.printTree()
				continue
			}
			// Queue plan edits and plain notes; they apply after the leaf.
			queued = append(queued, line)
			if isPlanEdit(line) {
				g.io.Printf("queued for after this leaf: %s\n", strings.TrimSpace(line))
			} else {
				g.io.Printf("the leaf is running; note queued (use /add, /redo, /pause for control)\n")
			}

		case question := <-g.askCh:
			g.io.Printf("\n? %s\n> ", question)
			select {
			case ans, ok := <-g.io.Lines():
				if !ok {
					g.unblockAsker("input closed")
					cancel()
					<-doneCh
					return errStdinClosed
				}
				g.answerCh <- ans
				if isPause(ans) {
					// The user paused instead of answering: the answer text was
					// still delivered so the worker can unwind, then the run
					// stops and saves.
					cancel()
					<-doneCh
					_ = g.o.checkpoint()
					g.o.events.Record("user_pause", "", map[string]any{"where": "ask"})
					g.io.Printf("paused and saved.\n")
					return ErrPaused
				}
			case <-ctx.Done():
				g.unblockAsker("run interrupted")
				cancel()
				<-doneCh
				return ctx.Err()
			}

		case <-ctx.Done():
			g.unblockAsker("run interrupted")
			cancel()
			<-doneCh
			return ctx.Err()
		}
	}
}

// unblockAsker wakes a worker that may be blocked in askHook waiting for an
// answer, so a canceled leaf can observe its context and exit instead of
// deadlocking the controller on <-doneCh. It is a no-op when no question is
// pending.
func (g *Guider) unblockAsker(reason string) {
	select {
	case g.answerCh <- reason + "; do not call ask again, finish with what you have":
	default:
	}
}

// handleExecLine applies one line typed during execution: /add, /redo, or a
// free-form instruction (which is added as a new leaf).
func (g *Guider) handleExecLine(ctx context.Context, line string) error {
	line = strings.TrimSpace(line)
	switch {
	case isPause(line):
		_ = g.o.checkpoint()
		g.o.events.Record("user_pause", "", map[string]any{"where": "idle"})
		return ErrPaused
	case line == "/plan":
		// Re-show the current tree so the user can review what remains,
		// without changing the execution flow.
		g.o.printTree()
		return nil
	case strings.HasPrefix(line, "/redo "):
		id := strings.TrimSpace(strings.TrimPrefix(line, "/redo"))
		g.io.Printf("resetting %s\n", id)
		return g.o.sched.UpdateNodeState(id, models.TaskStatePending, "user requested redo")
	case strings.HasPrefix(line, "/add"):
		instruction := strings.TrimSpace(strings.TrimPrefix(line, "/add"))
		return g.addLeaf(ctx, instruction)
	case strings.HasPrefix(line, "/"):
		return fmt.Errorf("unknown command %q (available: /add, /redo, /pause, /plan)", line)
	default:
		// Free text during a no-leaf-ready prompt: treat as a new leaf.
		return g.addLeaf(ctx, line)
	}
}

func (g *Guider) addLeaf(ctx context.Context, instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("/add needs an instruction")
	}
	base := sanitizeID(instruction)
	id := uniqueID(base, func(cand string) bool { return g.o.tree.HasNode(cand) })

	if _, err := g.o.tree.AddChild(g.o.tree.RootID, id, instruction, instruction, models.NodeTypeLeaf); err != nil {
		return err
	}
	// A new pending child means the root is no longer complete: move it back
	// to PENDING so it isn't accepted before the new leaf runs.
	if err := g.o.tree.UpdateNode(g.o.tree.RootID, func(n *models.TaskNode) error {
		n.State = models.TaskStatePending
		return nil
	}); err != nil {
		return err
	}
	g.io.Printf("\nadded leaf %s: %s\n", id, instruction)
	return g.o.checkpoint()
}

func isPlanEdit(s string) bool {
	return strings.HasPrefix(s, "/add") || strings.HasPrefix(s, "/redo")
}

func isApprove(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "/approve", "/start", "approve", "start", "go", "y", "yes", "开始", "执行", "确认", "可以":
		return true
	}
	return false
}

func isAbort(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "/abort", "/cancel", "abort", "cancel", "取消", "放弃":
		return true
	}
	return false
}

func isPause(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "/pause", "/stop", "pause", "stop", "暂停":
		return true
	}
	return false
}
