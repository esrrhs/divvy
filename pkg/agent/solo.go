package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// errSoloAlreadyComplete reports that the goal was already finished. It is not
// a failure — -resume on a done session simply has nothing to do — so RunSolo
// turns it into a clean exit.
var errSoloAlreadyComplete = errors.New("solo goal already complete")

// Solo mode drives the whole goal in one conversation.
//
// Tree mode splits the goal into contracted leaves and gives each a clean
// context. That is the right shape when the work exceeds a context window, but
// it has a structural hole: a leaf contract can only say "these files are my
// output". Whole-project properties — "standard library only", "these five
// test scenarios must exist" — belong to no leaf, so every leaf can satisfy its
// own DoD while the assembled deliverable violates the goal. A measured 66-leaf
// run spent 17.8M tokens and still shipped a third-party test dependency the
// goal forbade.
//
// Solo is the opposite trade: one context, one loop, and verification failures
// come back into the SAME conversation, so the model keeps everything it has
// learned. It costs one growing context instead of N clean ones, which is why
// it is a mode and not a replacement: use it when the task plausibly fits in
// one window, use the tree when it does not.

const (
	// soloDefaultSteps is the per-round tool-call budget. It is far larger
	// than a leaf's default (20) because a leaf is one atomic file and this is
	// a whole deliverable; running out mid-way is normal, not a failure.
	soloDefaultSteps = 80
	// soloDefaultRounds bounds how many repair rounds follow a failed
	// acceptance when -max-retries is 0.
	soloDefaultRounds = 3
	// soloContextChars is the conversation budget sent per request. The full
	// history is kept in memory and the middle is elided into a digest, so
	// what is dropped is always recoverable from the journal, never lost.
	soloContextChars = 100_000
)

// RunSolo executes the goal without decomposing it.
func (o *Orchestrator) RunSolo(ctx context.Context) (runErr error) {
	runStart := time.Now()
	o.recordSessionStart("solo")
	defer func() { o.recordSessionEnd("solo", runStart, runErr) }()

	ctx = o.startBudget(ctx)
	o.log.Banner("divvy (solo)")
	o.log.Infof("session %s", o.tree.ID)
	o.log.Infof("model   %s", o.cfg.Model)
	o.log.Infof("workdir %s", o.sandbox.Root)
	o.log.Infof("goal    %s", o.cfg.Goal)
	o.log.Infof("mode    one context, no decomposition")

	autoAcceptance, err := o.prepareSoloRoot()
	if errors.Is(err, errSoloAlreadyComplete) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, w := range o.layoutWarnings() {
		o.log.Warnf("layout check: %s", w)
	}
	o.printTree()
	if err := o.checkpoint(); err != nil {
		return err
	}

	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return fmt.Errorf("root node missing")
	}
	s := &soloSession{
		o:              o,
		sb:             o.sandbox,
		nodeID:         root.ID,
		budget:         o.soloContextBudget(),
		autoAcceptance: autoAcceptance,
		histPath:       o.storage.GetSoloHistoryPath(o.tree.ID),
	}
	system := llm.Message{Role: llm.RoleSystem, Content: soloSystemFor(o.cfg.WebEnabled, o.sandbox.DynamicToolDescriptions())}
	if prev, err := loadSoloHistory(s.histPath); err != nil || len(prev) < 2 {
		if err != nil {
			o.log.Warnf("earlier transcript unreadable, starting a new one: %v", err)
		}
		s.msgs = []llm.Message{system, {Role: llm.RoleUser, Content: o.soloTaskPrompt(o.sandbox, root)}}
	} else {
		// Resume: the transcript IS the state. Keep it, refresh the system
		// prompt (tool docs and rules may have changed), and tell the model
		// where the workspace stands now rather than replaying a stale
		// snapshot from the first turn.
		o.log.Infof("resuming: %d earlier messages from %s", len(prev), s.histPath)
		s.msgs = append(prev, llm.Message{Role: llm.RoleUser, Content: o.soloResumePrompt(root)})
		s.msgs[0] = system
	}

	steps := o.cfg.SoloSteps
	if steps < 1 {
		steps = soloDefaultSteps
	}
	rounds := o.cfg.SoloRounds
	if rounds < 1 {
		rounds = soloDefaultRounds
	}

	var lastFailure, stallNote string
	roundsRun := 0
	for round := 1; round <= rounds; round++ {
		roundsRun = round
		if err := ctx.Err(); err != nil {
			return err
		}
		o.log.Actionf("solo round %d/%d — budget %d tool calls", round, rounds, steps)
		// Acceptance tracks the workspace: it starts as a generic "something
		// was produced" check, and becomes the real toolchain command once the
		// model creates go.mod (or package.json, ...) inside this run.
		s.refreshAcceptance(root)

		summary, finished, err := s.runRound(ctx, steps)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if !finished {
			// Out of tool calls without a claim of completion. Verify anyway:
			// the work may already be done, and a passing acceptance is worth
			// more than the model's own opinion of its progress.
			o.log.Warnf("solo round %d used all %d tool calls without finish; verifying anyway", round, steps)
		}

		// Re-resolve right before verifying too: the model may have created
		// the project's marker file during this very round, and verifying a
		// placeholder acceptance would call a half-built deliverable done.
		s.refreshAcceptance(root)
		vr := o.verify(ctx, o.sandbox, root)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if vr.OK {
			if summary == "" {
				summary = "acceptance passed"
			}
			_ = s.checkpoint()
			return o.finishSolo(summary)
		}

		lastFailure = vr.Output
		o.recordNodeError(root.ID, vr.Output)
		o.events.Record("solo_verify_failed", root.ID, map[string]any{
			"round":   round,
			"command": vr.Command,
			"output":  clip(vr.Output, evReasonChars),
		})
		o.log.Warnf("solo round %d failed acceptance: %s", round, vr.Command)

		// Same failure twice means the previous repair did not address the
		// cause. Retrying in place a third time is what burned a measured run
		// from a 0.0985 penetration to a 0.3973 one: the model kept editing
		// the same lines with a fresh guess instead of changing strategy.
		if same := s.noteFailure(vr); same >= 2 {
			if o.cfg.MaxStall > 0 && same >= o.cfg.MaxStall {
				stallNote = fmt.Sprintf("the identical failure repeated %d times, so retrying in place was stopped early (raise -max-stall or -solo-rounds to continue)", same)
				o.log.Errorf("solo stalled: %s", stallNote)
				o.events.Record("solo_stall", root.ID, map[string]any{
					"run":    same,
					"limit":  o.cfg.MaxStall,
					"output": clip(vr.Output, evReasonChars),
				})
				break
			}
			o.log.Warnf("solo round %d failed the same way as the previous round; escalating to a strategy change", round)
			s.journal.escalation++
			o.events.Record("solo_escalate", root.ID, map[string]any{
				"run":     same,
				"command": vr.Command,
			})
		}

		// The whole point of solo: the failure lands in the conversation that
		// produced the code, so the model repairs it with full context instead
		// of a fresh leaf re-deriving the project from a contract.
		s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: soloRepairFor(vr, s.sameFailure)})
		s.journal.noteFailure(vr.Command)
		if err := s.checkpoint(); err != nil {
			return err
		}
	}

	msg := fmt.Sprintf("acceptance still failing after %d rounds", roundsRun)
	if stallNote != "" {
		msg = fmt.Sprintf("acceptance still failing after %d rounds: %s", roundsRun, stallNote)
	}
	if lastFailure != "" {
		msg += ": " + truncate(lastFailure, 1000)
	}
	o.log.Errorf("%s", msg)
	if err := o.sched.UpdateNodeState(o.tree.RootID, models.TaskStateFailed, msg); err != nil {
		return err
	}
	_ = s.checkpoint()
	// The node is marked FAILED above; the run itself must also report failure.
	// Returning the state transition's nil here would make the CLI print
	// "done" for a goal that never passed acceptance.
	return fmt.Errorf("%s", msg)
}

// prepareSoloRoot collapses the root into the single leaf that owns the goal.
// A tree session cannot be re-run this way: its children would be silently
// orphaned, and the work they describe never happens.
//
// The returned flag says whether acceptance is ours (toolchain defaults) or the
// caller's. Ours must be re-resolved as the project materialises; theirs is a
// contract and stays fixed.
func (o *Orchestrator) prepareSoloRoot() (autoAcceptance bool, err error) {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return false, fmt.Errorf("root node missing")
	}
	if len(root.ChildrenIDs) > 0 {
		return false, fmt.Errorf("session %s already has %d planned subtasks; -solo needs a fresh session (do not combine it with -resume of a tree run)",
			o.tree.ID, len(root.ChildrenIDs))
	}
	// A completed solo root is a finished goal, and the tree is one leaf, so
	// nothing else records that. Re-running it would rebuild a deliverable
	// that already passes its acceptance.
	if root.State == models.TaskStateCompleted && root.IntegrationVerified {
		o.log.Okf("session %s already completed this goal; nothing to do (start a new session to run it again)", o.tree.ID)
		return false, errSoloAlreadyComplete
	}
	switch {
	case len(o.cfg.DoDCommands) > 0:
		// An operator-supplied acceptance is a contract, not a guess: it does
		// not follow the workspace and is never re-resolved.
		root.DoD = models.DoD{
			Commands:   append([]string(nil), o.cfg.DoDCommands...),
			TimeoutSec: soloAcceptanceTimeout,
		}
	case len(root.DoD.Commands) == 0:
		autoAcceptance = true
		root.DoD = models.DoD{TimeoutSec: soloAcceptanceTimeout}
		soloAcceptanceFor(&root.DoD, tools.DetectProject(o.sandbox.Root))
	}
	err = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.Type = models.NodeTypeLeaf
		n.State = models.TaskStateRunning
		n.IntegrationVerified = false
		n.ErrorMsg = ""
		n.DoD = root.DoD
		return nil
	})
	return autoAcceptance, err
}

// soloAcceptanceTimeout is generous on purpose: a solo acceptance runs the
// whole suite of a deliverable it just wrote, not one package's tests.
const soloAcceptanceTimeout = 300

// soloAcceptanceFor fills the default acceptance for a solo run.
//
// It differs from the tree's goal DoD on purpose. The tree's default includes
// "go build .", which fails whenever the module root holds no Go files — a
// legitimate layout — because it is there to prove an entry point exists. Solo
// has no integration leaf to route that failure to, so a false negative would
// just burn rounds. "go test ./..." already compiles every package.
func soloAcceptanceFor(dod *models.DoD, proj tools.ProjectType) {
	dod.TimeoutSec = soloAcceptanceTimeout
	switch proj {
	case tools.ProjectGo, tools.ProjectNode, tools.ProjectRust, tools.ProjectPython:
		dod.Commands = defaultDoDCommands(nil, proj)
	default:
		// Nothing identifiable yet. "ls" would pass on an empty directory, so
		// require that the run actually produced a file.
		dod.Commands = []string{"find . -type f -not -path './.git/*' | grep -q ."}
	}
}

// refreshAcceptance re-resolves an auto-generated acceptance against the
// workspace as it is now, and tells the model when it changed. Without this a
// run that starts in an empty directory verifies "ls" and declares victory.
func (s *soloSession) refreshAcceptance(root *models.TaskNode) {
	if !s.autoAcceptance {
		return
	}
	before := strings.Join(root.DoD.Commands, " && ")
	next := *root
	soloAcceptanceFor(&next.DoD, tools.DetectProject(s.sb.Root))
	after := strings.Join(next.DoD.Commands, " && ")
	if after == before {
		return
	}
	root.DoD = next.DoD
	_ = s.o.tree.UpdateNode(root.ID, func(n *models.TaskNode) error {
		n.DoD = next.DoD
		return nil
	})
	s.o.log.Infof("solo acceptance updated: %s", after)
	s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
		"The workspace now looks like a %s project, so the acceptance that runs when you finish is:\n%s\nEvery command must pass before you call finish.",
		tools.DetectProject(s.sb.Root), after)})
}

func (o *Orchestrator) soloContextBudget() int {
	if o.cfg.SoloContextChars > 0 {
		return o.cfg.SoloContextChars
	}
	return soloContextChars
}

func (o *Orchestrator) finishSolo(summary string) error {
	_ = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.ResultSummary = summary
		n.ErrorMsg = ""
		return nil
	})
	if err := o.markAcceptancePassed(); err != nil {
		return err
	}
	if err := o.sched.UpdateNodeState(o.tree.RootID, models.TaskStateCompleted, ""); err != nil {
		return err
	}
	// Tree mode persists through checkpoints scattered across its scheduler
	// loop; solo has none, so the final state must be written here or -status
	// and -report read a stale PENDING root.
	if err := o.checkpoint(); err != nil {
		return err
	}
	o.log.Okf("goal complete — %s", truncate(summary, 160))
	return nil
}

// soloSession is one conversation that survives across repair rounds.
type soloSession struct {
	o      *Orchestrator
	sb     *tools.Sandbox
	nodeID string
	budget int // approximate character budget for one request
	msgs   []llm.Message
	// autoAcceptance marks a DoD this package filled in (rather than one the
	// caller supplied), which is the only case where re-resolving it against
	// the evolving workspace is legitimate.
	autoAcceptance bool
	journal        soloJournal
	// histPath is where the transcript is written so an interrupted run
	// resumes its own conversation instead of rebuilding from a workspace it
	// has no memory of.
	histPath string

	// failureFP / sameFailure track repeated acceptance failures across
	// rounds. Fingerprints come from stall.go, so "penetrated by 0.0985" and
	// "penetrated by 0.3973" compare equal: same failure mode, different
	// number. That is exactly the case where retrying in place is wasted.
	failureFP   string
	sameFailure int

	// stepFP / stepSame track the same thing *inside* one round. A 60-step
	// round is long enough for the model to guess-edit-test in a loop without
	// ever reaching the round boundary where failures get reviewed.
	stepFP   string
	stepSame int
	stepNags int
}

// soloStepNags caps how many times one round interrupts the guess loop. Two is
// enough to reframe the problem; a third is just noise in the transcript.
const soloStepNags = 2

// trackStepFailure watches for the same command failing the same way twice in a
// row within one round, and returns a message that interrupts the loop.
//
// Round-level detection alone is too coarse: a measured run spent 38 of its 60
// steps alternating edit/test on one assertion whose failure value oscillated
// between -0.15 and -1953 without ever converging, because each edit was a new
// guess rather than an observation.
func (s *soloSession) trackStepFailure(step int, act taggedAction, out string, callErr error) (string, bool) {
	if act.Name != tools.ToolRunBash || s.stepNags >= soloStepNags {
		return "", false
	}
	if callErr == nil && strings.HasPrefix(out, "exit_code: 0\n") {
		s.stepFP, s.stepSame = "", 0
		return "", false
	}
	cmd, _ := stringFromArgs(act.Args, "command")
	fp := stallFingerprint(s.o.projectType(), strings.TrimSpace(cmd)+"\n"+out)
	if fp == "" {
		return "", false
	}
	if fp == s.stepFP {
		s.stepSame++
	} else {
		s.stepFP, s.stepSame = fp, 1
	}
	if s.stepSame < 2 {
		return "", false
	}
	// Reset so the next identical pair gets a fresh count; cap the nags.
	s.stepFP, s.stepSame = "", 0
	s.stepNags++
	s.journal.escalation++
	s.o.log.Warnf("solo step %d: the same command failed the same way twice; interrupting the guess loop", step)
	s.o.events.Record("solo_step_stall", s.nodeID, map[string]any{
		"step":    step,
		"command": clip(cmd, evReasonChars),
	})
	return soloStepIntervention(cmd), true
}

// soloStepIntervention reframes the problem when an edit changed nothing
// observable. It is deliberately about method, not about any domain: observe,
// then fix what the observation points at.
func soloStepIntervention(cmd string) string {
	return fmt.Sprintf(`Stop. %q just failed with the same output as your previous attempt. Whatever you changed between those two runs made no observable difference, so another edit made the same way will not either.

Before touching the code again:
1. Write down the exact value that is wrong and the value you expected.
2. Observe it: print the intermediate values around the failure, or drive the failing scenario from a small scratch program that prints state step by step. Keep the scratch program inside the workspace (for example scratch/main.go) so the toolchain can resolve the project's own packages — a file written outside the project cannot import them. Delete it before you call finish.
3. If the observation contradicts what you assumed the code was doing, that contradiction is the bug. Fix that, not the number in the assertion.
4. Re-run %q. If the output changes you are making progress; if it does not, the theory is still wrong.

Do not weaken or delete the check to make it pass.`, cmd, cmd)
}

// checkpoint persists both halves of solo's state: the tree (so -status sees
// the truth) and the transcript (so -resume continues instead of restarting).
// A transcript write failure is never fatal — it costs memory, not correctness.
func (s *soloSession) checkpoint() error {
	if err := saveSoloHistory(s.histPath, s.msgs); err != nil {
		s.o.log.Warnf("solo transcript not saved: %v", err)
	}
	return s.o.checkpoint()
}

// noteFailure folds one failed acceptance into the streak and returns its new
// length (1 = a new failure mode). A fingerprint is only comparable when the
// command that failed is the same, so a changed acceptance resets the streak.
func (s *soloSession) noteFailure(vr VerifyResult) int {
	fp := stallFingerprint(s.o.projectType(), vr.Command+"\n"+vr.Output)
	switch {
	case fp == "":
		s.failureFP, s.sameFailure = "", 1
	case fp == s.failureFP:
		s.sameFailure++
	default:
		s.failureFP, s.sameFailure = fp, 1
	}
	return s.sameFailure
}

// runRound runs up to steps tool calls. finished reports that the model called
// finish (not that the work is correct — acceptance decides that).
func (s *soloSession) runRound(ctx context.Context, steps int) (summary string, finished bool, err error) {
	o := s.o
	var native []llm.Tool
	if o.cfg.NativeTools {
		native = nativeToolDefs(s.sb)
	}

	for step := 1; step <= steps; step++ {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		o.log.Actionf("solo step %d/%d", step, steps)

		resp, err := o.chat(ctx, "solo", s.nodeID, llm.Request{
			Model:       o.cfg.Model,
			Messages:    o.stepBudgetMessages(s.compose(step, steps), step, steps),
			Tools:       native,
			Temperature: o.cfg.Temperature,
			MaxTokens:   o.cfg.MaxTokens,
			Stream:      o.cfg.Stream,
		})
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			return "", false, err
		}
		o.log.Debugf("solo raw: %s", truncate(resp.Content, 400))

		actions := collectActions(resp)
		if len(actions) == 0 {
			s.msgs = append(s.msgs,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Content},
				llm.Message{Role: llm.RoleUser, Content: `Could not parse a tool call. Reply with ONLY JSON: {"thought":"...","action":"tool_name","args":{}}`},
			)
			continue
		}
		s.msgs = append(s.msgs, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		for _, act := range actions {
			o.log.Actionf("%s %s %s", s.nodeID, act.Name, previewArgs(act))

			if act.Name == tools.ToolFinish {
				if blocked, ok := s.finishGate(ctx); !ok {
					s.appendToolResult(act, blocked)
					continue
				}
				summary, _ = stringFromArgs(act.Args, "summary")
				if summary == "" {
					summary = act.Thought
				}
				if summary == "" {
					summary = "finished"
				}
				return summary, true, nil
			}

			if act.Name == "ask" {
				answer := o.answerAsk(act)
				s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("User answer:\n%s", answer)})
				continue
			}

			callStart := time.Now()
			out, callErr := s.sb.Call(ctx, act.Name, act.Args)
			o.events.ToolCall(s.nodeID, act.Name, act.Args, out, callErr, callStart)
			if callErr != nil {
				out = "ERROR: " + callErr.Error()
				o.log.Warnf("%s", out)
			} else {
				o.log.Debugf("tool out: %s", truncate(out, 240))
			}
			s.journal.note(act, out, callErr)
			s.appendToolResult(act, out)
			if msg, interrupted := s.trackStepFailure(step, act, out, callErr); interrupted {
				s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: msg})
			}

			// Checkpoint periodically: a solo session is long, and losing the
			// token accounting (the only persisted state) on a crash makes the
			// budget look untouched on resume.
			if step%10 == 0 {
				_ = s.checkpoint()
			}
		}
	}
	_ = s.checkpoint()
	return "", false, nil
}

// finishGate applies the same deterministic pre-finish review a leaf gets
// (conflict markers, hard-coded secrets). ok=false carries the message the
// model must see before it tries again.
func (s *soloSession) finishGate(ctx context.Context) (string, bool) {
	gate, err := s.sb.PreFinishGate(ctx)
	if err != nil {
		s.o.log.Debugf("pre-finish review unavailable: %v", err)
		return "", true
	}
	if gate == nil {
		return "", true
	}
	s.o.events.Record("finish_gate", s.nodeID, map[string]any{
		"blocking": gate.Blocking,
		"report":   clip(gate.Report, evReasonChars),
	})
	if !gate.Blocking {
		s.o.log.Warnf("pre-finish warnings:\n%s", gate.Report)
		return "", true
	}
	s.o.log.Warnf("%s finish rejected by review gate", s.nodeID)
	return "finish REJECTED by review_diff. Fix these blocking issue(s) before finishing " +
		"(conflict markers and hard-coded secrets are never accepted):\n" + gate.Report +
		"\nFix the code, then call finish again. Debug-print and large-diff items are warnings only, " +
		"but conflict/secret items must be removed.", false
}

func (o *Orchestrator) answerAsk(act taggedAction) string {
	question, _ := stringFromArgs(act.Args, "question")
	if o.askHook == nil {
		return "No interactive user is available in this run. Make a reasonable assumption and continue; do not call ask again."
	}
	o.log.Actionf("asks: %s", question)
	answer := o.askHook(question)
	o.events.Record("ask", "", map[string]any{
		"question": clip(question, evReasonChars),
		"answer":   clip(answer, evReasonChars),
	})
	return answer
}

func (s *soloSession) appendToolResult(act taggedAction, out string) {
	if s.o.cfg.NativeTools && act.ID != "" {
		s.msgs = append(s.msgs, llm.Message{Role: llm.RoleTool, ToolCallID: act.ID, Name: act.Name, Content: out})
		return
	}
	s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("Tool %s result:\n%s", act.Name, out)})
}

// compose returns the message window for this step: the system prompt, the
// goal, a digest of everything elided so far, and as much recent history as
// fits. The digest is deterministic — it is built from the journal, not by
// asking the model to summarise — so compaction costs no tokens and cannot
// hallucinate what happened.
func (s *soloSession) compose(step, steps int) []llm.Message {
	head := s.msgs[:2]
	body := s.msgs[2:]

	used := len(head[0].Content) + len(head[1].Content)
	digest := s.journal.render(len(body))
	tail, dropped := tailWithinChars(body, s.budget-used-len(digest)-2000)

	out := make([]llm.Message, 0, len(tail)+3)
	out = append(out, head...)
	if dropped > 0 {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: digest})
	}
	out = append(out, tail...)
	return out
}

// tailWithinChars returns the longest suffix of msgs that fits in budget,
// moved back to a boundary that never orphans a tool result. dropped counts
// the messages left out.
func tailWithinChars(msgs []llm.Message, budget int) (tail []llm.Message, dropped int) {
	if budget < 0 {
		budget = 0
	}
	total := 0
	cut := len(msgs)
	for cut > 0 {
		size := messageChars(msgs[cut-1])
		if total+size > budget && cut < len(msgs) {
			break
		}
		total += size
		cut--
	}
	// Never start the tail on a tool reply: its assistant turn would be gone
	// and OpenAI-compatible servers reject the request with 400.
	for cut > 0 && splitsToolGroup(msgs, cut) {
		cut--
	}
	return msgs[cut:], cut
}

func messageChars(m llm.Message) int {
	n := len(m.Content) + 16
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Arguments) + len(tc.Function.Name) + 16
	}
	return n
}

// soloJournal is the deterministic memory of a solo run: what got written and
// which commands failed. It is rendered only when history is elided, so the
// model never loses the thread of its own work.
type soloJournal struct {
	files      []string
	deletes    []string
	cmds       int
	fails      []string
	calls      int
	escalation int
}

func (j *soloJournal) note(act taggedAction, out string, callErr error) {
	j.calls++
	switch act.Name {
	case tools.ToolWriteFile, tools.ToolReplaceLines:
		if p, ok := stringFromArgs(act.Args, "path"); ok {
			j.addFile(p)
		}
	case tools.ToolDeletePath:
		if p, ok := stringFromArgs(act.Args, "path"); ok {
			j.addDelete(p)
		}
	case tools.ToolMovePath:
		if to, ok := stringFromArgs(act.Args, "to"); ok {
			j.addFile(to)
		}
	case tools.ToolRunBash:
		j.cmds++
		if callErr != nil || strings.HasPrefix(out, "ERROR:") || strings.Contains(out, "exit status") || strings.Contains(out, "exit 1") {
			if cmd, ok := stringFromArgs(act.Args, "command"); ok {
				j.noteFailure(cmd)
			}
		}
	}
}

func (j *soloJournal) noteFailure(s string) {
	j.fails = append(j.fails, truncate(strings.TrimSpace(s), 200))
	if len(j.fails) > 5 {
		j.fails = j.fails[len(j.fails)-5:]
	}
}

func (j *soloJournal) addFile(p string) {
	for _, f := range j.files {
		if f == p {
			return
		}
	}
	j.files = append(j.files, p)
}

func (j *soloJournal) addDelete(p string) {
	j.deletes = append(j.deletes, p)
}

// render describes the work behind the elided window.
func (j *soloJournal) render(elided int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[progress log] %d earlier message(s) were elided to fit the context window. ", elided)
	fmt.Fprintf(&b, "This run has made %d tool call(s), %d of them shell commands.\n", j.calls, j.cmds)
	if len(j.files) > 0 {
		shown := j.files
		suffix := ""
		if len(shown) > 24 {
			shown = shown[len(shown)-24:]
			suffix = fmt.Sprintf(" (showing the last 24 of %d)", len(j.files))
		}
		fmt.Fprintf(&b, "Files created or edited%s: %s\n", suffix, strings.Join(shown, ", "))
	}
	if len(j.deletes) > 0 {
		fmt.Fprintf(&b, "Paths deleted: %s\n", strings.Join(j.deletes, ", "))
	}
	if len(j.fails) > 0 {
		fmt.Fprintf(&b, "Recent failing commands / errors:\n")
		for _, f := range j.fails {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if j.escalation > 0 {
		fmt.Fprintf(&b, "You were already told %d time(s) that editing in place was not working and to observe before changing code.\n", j.escalation)
	}
	b.WriteString("Re-read any file you need before editing it; the log above is a summary, not the content.")
	return b.String()
}

// soloTaskPrompt is the whole brief: the goal, what will be checked, and the
// rules that keep a single-context run from quietly dropping requirements.
func (o *Orchestrator) soloTaskPrompt(sb *tools.Sandbox, root *models.TaskNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n\n", sb.Root)
	fmt.Fprintf(&b, "Goal:\n%s\n\n", o.tree.Goal)
	b.WriteString("Acceptance that will run after you call finish:\n")
	b.WriteString(formatDoD(root.DoD))
	b.WriteString("\n\nWorkspace files right now:\n")
	snap := sb.Snapshot()
	b.WriteString(snap)
	if workspaceHasFiles(snap) {
		// Measured failure mode: handed a goal whose workspace already held a
		// passing implementation, a solo run rewrote every file from scratch —
		// it read the goal's "from scratch" wording as "ignore what is here".
		// The rule is general: read before you replace.
		b.WriteString(`
The workspace is NOT empty. The entries above may be a partial or even a complete previous attempt at this goal. Treat them as work to check, not as a blank slate:
- Run the acceptance commands first. Whatever already passes does not need to be rewritten.
- Read a file before you replace it. Overwriting a file you have not read throws away work that may already be correct, and costs the budget of rebuilding it.
- Fix the part that is wrong and keep the part that is right.`)
	}
	b.WriteString(`
How this run works:
- You own the goal from start to finish. Nothing runs after you: no reviewer, no later task that will fix what you skip.
- Every sentence of the goal is a requirement, including negative ones ("standard library only", "no third-party dependencies") and required test scenarios. The acceptance commands are a floor, not the definition of done. When you finish, ask yourself: if a strict reviewer compared the goal text with what is on disk, what would they reject?
- Before writing code, list the goal's requirements to yourself, then implement them in small verified increments: one package, run its test, then the next.
- Run the acceptance commands yourself before calling finish. Do not claim done on the strength of "it compiles".
- If acceptance fails after you finish, you will get another turn in this same conversation with the error. Keep going from there.

Start now.`)
	return b.String()
}

// soloResumePrompt opens a resumed solo turn. The transcript above is the
// model's own earlier work, but its opening snapshot is stale, so the current
// one is restated here — otherwise the model plans against a workspace that no
// longer matches the disk.
func (o *Orchestrator) soloResumePrompt(root *models.TaskNode) string {
	var b strings.Builder
	b.WriteString("This run was interrupted and is now resuming. Everything above is your own earlier work in this same conversation; the workspace is as you left it.\n\n")
	b.WriteString("Acceptance that will run after you call finish:\n")
	b.WriteString(formatDoD(root.DoD))
	b.WriteString("\n\nWorkspace files right now:\n")
	b.WriteString(o.sandbox.Snapshot())
	b.WriteString(`
Do not restart from scratch. Run the acceptance commands first to see where you actually are, then continue from that point. What already passes does not need to be rewritten.`)
	return b.String()
}

// workspaceHasFiles reports whether a snapshot describes a non-empty workspace.
// ListDir answers "" or "(empty)" for an empty directory and an error sentence
// when it could not read at all — in that case assume there is something there
// and let the model look.
func workspaceHasFiles(snap string) bool {
	t := strings.TrimSpace(snap)
	return t != "" && t != "(empty)" && !strings.HasPrefix(t, "(unable to list workspace")
}

func soloRepairFor(vr VerifyResult, same int) string {
	if same < 2 {
		return soloRepairPrompt(vr)
	}
	return soloEscalationPrompt(vr, same)
}

// soloEscalationPrompt is the second-repetition message. Plain "fix it again"
// does not work once the model has already guessed twice: the failing case is
// usually a wrong mental model, not a missing line, so the demand is to
// observe before editing. The wording is deliberately project-agnostic.
func soloEscalationPrompt(vr VerifyResult, same int) string {
	return fmt.Sprintf(`Acceptance FAILED again, the same way as the previous %d rounds. Exit code %d from: %s

%s

Your last fix did not change the outcome. Do NOT edit the same lines again on another hunch, and do NOT loosen the assertion to make it pass — that is how the previous attempt made it worse.

Change strategy:
1. Say in one sentence what you believe the cause is, and what observation would confirm or refute it.
2. Observe it. Print the intermediate values around the failure, add a temporary debug output, or run the failing scenario in a small scratch program that prints state step by step. Keep that program inside the workspace (for example scratch/main.go) so the toolchain can resolve the project's own packages.
3. If your observation contradicts your theory, discard the theory and form a new one. Do not tune numbers until something passes.
4. Only after the observation explains the failure, change the code at the place the observation points to.
5. Re-run the acceptance command yourself.

The full history of what you built is still in this conversation. Keep everything that already works.`,
		same, vr.ExitCode, vr.Command, truncate(vr.Output, 6000))
}

func soloRepairPrompt(vr VerifyResult) string {
	return fmt.Sprintf(`Acceptance FAILED. Exit code %d from: %s

%s

Fix the code so this passes. Do not restart from scratch: the full history of what you already built is still in this conversation. After fixing, run the acceptance command yourself, then call finish.`,
		vr.ExitCode, vr.Command, truncate(vr.Output, 6000))
}

const soloMarker = "You are a coding agent that owns a whole goal end to end."

// soloSystemFor returns the solo system prompt. It reuses the worker's tool
// documentation (the tools are the same) and swaps the "one atomic task" rules
// for whole-goal rules. The wording about isolation is rewritten rather than
// inherited: solo runs directly in the project workspace, with no mirror.
func soloSystemFor(web bool, dynamicTools string) string {
	base := strings.Replace(workerSystemBase, workerMarker, soloMarker, 1)
	base = strings.Replace(base, "You work inside an isolated workspace.",
		"You work directly in the project workspace. Every file you write is the delivered file.", 1)
	rules := soloRulesOffline
	if web {
		rules = soloRulesWeb
	}
	return base + dynamicTools + rules
}

const soloRulesOffline = `

Rules for this run:
- You are the only agent. There is no task split and no one to clean up after you: if you skip a requirement, it stays skipped.
- Build the goal in small verified increments: write one package, run its test, move on. Do not write everything and test at the end.
- Re-read a file before editing it when you are unsure of its current contents; your memory of it may be older than the file.
- Stay inside the workspace. Do not access the network.
- In a git repository, finish is automatically checked via review_diff: unresolved conflict markers or hard-coded secrets REJECT finish and send the report back to you.
- Call finish only when the goal is actually satisfied and the acceptance commands pass locally.
- Never wrap JSON in markdown.
- One action per turn.
`

const soloRulesWeb = `

Rules for this run:
- You are the only agent. There is no task split and no one to clean up after you: if you skip a requirement, it stays skipped.
- Build the goal in small verified increments: write one package, run its test, move on. Do not write everything and test at the end.
- Re-read a file before editing it when you are unsure of its current contents; your memory of it may be older than the file.
- Stay inside the workspace for files. Network access is limited to web_search/web_fetch for public pages the goal genuinely needs.
- In a git repository, finish is automatically checked via review_diff: unresolved conflict markers or hard-coded secrets REJECT finish and send the report back to you.
- Call finish only when the goal is actually satisfied and the acceptance commands pass locally.
- Never wrap JSON in markdown.
- One action per turn.
`
