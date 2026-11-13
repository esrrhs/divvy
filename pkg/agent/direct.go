package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

const (
	// directDefaultSteps is the per-turn tool-call budget. A turn is a
	// user follow-up in a persistent conversation, not a single leaf, so
	// it needs more room than the leaf default of 20.
	directDefaultSteps = 40
	// directDefaultContextChars is the window sent to the model. The full
	// transcript stays on disk and is only compressed when composing a request.
	directDefaultContextChars = 100_000
	directMapMaxEntries       = 80
	directMapMaxDepth         = 3
	// directRepeatLimit is how many identical tool calls in a row count as a
	// loop. Two would fire on a legitimate read-then-verify pair; four wastes
	// a step on something already unmistakable.
	directRepeatLimit = 3
	// directRepeatNags caps how many times one turn interrupts a repeated-call
	// loop. The step budget still bounds the turn after that.
	directRepeatNags = 2
	// directFailureLimit is how many times the same verification command may
	// fail with the same output before the loop demands a strategy change.
	// Solo escalates on the second identical failure; direct is conversational,
	// so it gets the same one-observation-round budget.
	directFailureLimit = 2
	// directFailureNags caps how many strategy-change prompts one turn gets,
	// so the intervention itself cannot become the loop.
	directFailureNags = 2
)

// directMarker is the leading sentence of the direct-mode system prompt so
// tests and logs can recognize this kernel (distinct from the worker/solo
// markers).
const directMarker = "You are a coding agent in a persistent project conversation."

// DirectControl is the run-handle surface the loop uses for mid-run inject,
// pause-at-boundary, and live status. Tests may pass nil.
type DirectControl interface {
	TakePendingInject() (text, id string)
	PauseRequested() bool
	SetInTool(bool)
	SetSummary(string)
}

// DirectAgent is the persistent tool-call loop used by conversation
// "direct" mode: read/search, edit, run verification, repair from real
// tool results, then emit the model's own finish summary. It is not a
// task-tree wrapper and does not treat a full-turn rerun as "continuing".
type DirectAgent struct {
	o   *Orchestrator
	ctl DirectControl
	st  *directState
	sb  *tools.Sandbox
	mir *tools.Mirror

	// invalidRounds counts consecutive empty/unparseable model replies so a
	// weak model cannot burn the whole per-turn step budget on
	// "please reply with JSON" retries. Reset as soon as an action parses.
	invalidRounds int
	// repeatKey / repeatCount / repeatNags catch the other loop shape: the
	// exact same tool call repeated with identical arguments (observed as ten
	// identical read_file calls), which never fails and so never trips a
	// failure-based guard.
	repeatKey   string
	repeatCount int
	repeatNags  int
	// failKey / failCount / failNags catch the third loop shape: different
	// calls (edits interleaved with the same verification command) that keep
	// producing the same failure. The workspace changes, the outcome does not.
	failKey   string
	failCount int
	failNags  int
}

func NewDirect(o *Orchestrator, ctl DirectControl) *DirectAgent {
	return &DirectAgent{o: o, ctl: ctl}
}

const directReviewNodeID = "direct"

func (d *DirectAgent) toolSandbox() *tools.Sandbox {
	if d.sb != nil {
		return d.sb
	}
	return d.o.sandbox
}

func (d *DirectAgent) keepMirror(runErr error) bool {
	if d.mir == nil || d.st == nil || d.st.Done {
		return false
	}
	if errors.Is(runErr, ErrPaused) || errors.Is(runErr, context.Canceled) {
		return true
	}
	if d.st.PendingApproval {
		return true
	}
	// Unexpected stop mid-run: keep unmerged edits so a resume sees the
	// same isolated workspace the transcript describes.
	if runErr != nil {
		return true
	}
	return false
}

func (d *DirectAgent) mirrorDir() string {
	id := "session"
	if d.o != nil && d.o.tree != nil && d.o.tree.ID != "" {
		id = d.o.tree.ID
	}
	data := ".divvy"
	if d.o != nil && strings.TrimSpace(d.o.cfg.DataDir) != "" {
		data = d.o.cfg.DataDir
	}
	return filepath.Join(data, "mirrors", id)
}

func (d *DirectAgent) setupIsolation() error {
	d.sb = d.o.sandbox
	if d.o.cfg.LeafApprovalMode() != LeafApprovalManual {
		return nil
	}
	if d.st != nil && d.st.Mirror != nil && strings.TrimSpace(d.st.Mirror.Dir) != "" {
		m, err := tools.OpenMirror(*d.st.Mirror)
		if err == nil {
			return d.adoptMirror(m)
		}
		d.o.log.Warnf("isolated workspace could not be reopened (%v); starting a fresh snapshot — unmerged edits from the previous attempt were not applied", err)
		d.st.Mirror = nil
		d.st.PendingApproval = false
	}
	m, err := tools.NewMirrorAt(d.o.sandbox.Root, d.mirrorDir())
	if err != nil {
		return fmt.Errorf("create isolated workspace for manual approval: %w", err)
	}
	return d.adoptMirror(m)
}

func (d *DirectAgent) adoptMirror(m *tools.Mirror) error {
	sb, err := m.Sandbox()
	if err != nil {
		m.Close()
		return err
	}
	if d.o.sandbox != nil {
		sb.Web = d.o.sandbox.Web
		sb.Browser = d.o.sandbox.Browser
		sb.HideEnv(d.o.sandbox.HiddenEnv()...)
		d.o.sandbox.CopyLimits(sb)
	}
	d.applyPolicyToSandbox(sb)
	d.mir = m
	d.sb = sb
	if d.st != nil {
		st := m.State()
		d.st.Mirror = &st
	}
	d.o.log.Infof("isolated workspace: %s (merges only after approval)", tools.TrimPath(m.Dir))
	return d.persist()
}

func (d *DirectAgent) closeMirror() {
	if d.mir != nil {
		d.mir.Close()
		d.mir = nil
	}
	if d.o != nil {
		d.sb = d.o.sandbox
	}
	if d.st != nil {
		d.st.Mirror = nil
		d.st.PendingApproval = false
	}
}

func (d *DirectAgent) reviewAndMaybeMerge(ctx context.Context, summary string) error {
	if d.o.cfg.LeafApprovalMode() != LeafApprovalManual || d.mir == nil {
		return nil
	}
	changes, err := d.mir.PreviewChanges()
	if err != nil {
		return fmt.Errorf("preview changes for review: %w", err)
	}
	if len(changes) == 0 {
		d.closeMirror()
		return d.persist()
	}
	hook := d.o.leafApproval
	if hook == nil {
		return fmt.Errorf("manual approval is required but no review hook is installed; changes were not written to the project workspace")
	}
	d.st.PendingApproval = true
	if summary != "" {
		d.st.Summary = summary
	}
	_ = d.persist()
	d.o.events.Record("leaf_approval", directReviewNodeID, map[string]any{
		"phase": "pending", "files": len(changes), "title": "本轮改动",
	})
	decision, aerr := hook(ctx, LeafApprovalRequest{
		NodeID:  directReviewNodeID,
		Title:   "本轮改动",
		Changes: changes,
	})
	if aerr != nil {
		if errors.Is(aerr, ErrPaused) || errors.Is(aerr, context.Canceled) || ctx.Err() != nil {
			if errors.Is(aerr, ErrPaused) || d.pauseRequested() {
				return ErrPaused
			}
			return aerr
		}
		return aerr
	}
	if !decision.Approved {
		d.o.events.Record("leaf_approval", directReviewNodeID, map[string]any{
			"phase": "rejected", "comment": clip(decision.Comment, evReasonChars),
		})
		msg := "本轮改动已被拒绝，未写入工作区。"
		if c := strings.TrimSpace(decision.Comment); c != "" {
			msg += " 原因：" + c
		}
		d.closeMirror()
		d.st.PendingApproval = false
		return d.finish(msg)
	}
	d.o.events.Record("leaf_approval", directReviewNodeID, map[string]any{"phase": "approved"})
	if err := applyLeafApprovalPatches(d.mir, changes, decision); err != nil {
		return fmt.Errorf("apply hunk patches before merge: %w", err)
	}
	merged, deleted, merr := d.mir.MergeBack()
	if merr != nil {
		var conflict *tools.MergeConflictError
		if errors.As(merr, &conflict) {
			d.o.events.Record("leaf_approval", directReviewNodeID, map[string]any{
				"phase": "blocked", "reason": "external_conflict", "paths": conflict.Paths,
			})
			msg := "未写入工作区：审批通过后发现工作区文件已被外部修改（" + strings.Join(conflict.Paths, ", ") + "），为避免覆盖这些改动，本轮结果未合并。"
			d.closeMirror()
			d.st.PendingApproval = false
			return d.finish(msg)
		}
		return fmt.Errorf("merge approved changes: %w", merr)
	}
	d.o.log.Infof("approved merge: wrote %d, deleted %d", len(merged), len(deleted))
	d.closeMirror()
	d.st.PendingApproval = false
	return d.persist()
}

func (d *DirectAgent) transcriptPath() string {
	if p := strings.TrimSpace(d.o.cfg.TranscriptPath); p != "" {
		return p
	}
	return d.o.storage.GetDirectHistoryPath(d.o.tree.ID)
}

func (d *DirectAgent) contextBudget() int {
	if d.o.cfg.DirectContextChars > 0 {
		return d.o.cfg.DirectContextChars
	}
	if d.o.cfg.SoloContextChars > 0 {
		return d.o.cfg.SoloContextChars
	}
	return directDefaultContextChars
}

func (d *DirectAgent) maxSteps() int {
	if d.o.cfg.DirectSteps > 0 {
		return d.o.cfg.DirectSteps
	}
	return directDefaultSteps
}

// Run executes (or resumes) the direct tool loop until the model finishes,
// the budget is exhausted, or the run is paused/cancelled.
func (d *DirectAgent) Run(ctx context.Context) (runErr error) {
	runStart := time.Now()
	d.o.recordSessionStart("direct")
	defer func() { d.o.recordSessionEnd("direct", runStart, runErr) }()

	ctx = d.o.startBudget(ctx)
	d.o.log.Banner("divvy (direct)")
	d.o.log.Infof("session %s", d.o.tree.ID)
	d.o.log.Infof("model   %s", d.o.cfg.Model)
	d.o.log.Infof("workdir %s", d.o.sandbox.Root)
	d.o.log.Infof("mode    persistent tool loop, no task-tree planning")

	if err := d.prepareRoot(); err != nil {
		return err
	}

	st, err := loadDirectState(d.transcriptPath(), d.o.cfg.APIKey)
	if err != nil {
		return err
	}
	d.applySandboxPolicy()
	system := llm.Message{Role: llm.RoleSystem, Content: directSystemGuide(d.o.sandbox, d.o.cfg.DataDir)}
	switch {
	case st == nil || len(st.Messages) < 2:
		st = &directState{Version: 1, Messages: []llm.Message{system, {Role: llm.RoleUser, Content: d.openingPrompt()}}}
	case st.Done:
		st.Messages[0] = system
		st.Done = false
		st.Summary = ""
		st.InFlight = nil
		st.Step = 0
		st.PendingAsk = ""
		st.PendingAskID = ""
		st.PendingApproval = false
		st.PendingActions = nil
		st.Mirror = nil
		st.Messages = append(st.Messages, llm.Message{Role: llm.RoleUser, Content: d.followUpPrompt()})
	default:
		// Resume: the transcript is the state. Do not replay tool side
		// effects. If a call was in flight, tell the model to inspect
		// the workspace instead of blindly repeating it.
		st.Messages[0] = system
		if st.InFlight != nil {
			if st.InFlight.ID == "" {
				st.InFlight.ID = inferOpenToolCallID(st.Messages, st.InFlight.Name)
			}
			st.Messages = append(st.Messages, inFlightRecoveryMessage(st.InFlight, d.o.cfg.NativeTools))
			d.o.events.Record("direct_inflight_recovered", d.o.tree.RootID, map[string]any{
				"tool": st.InFlight.Name,
			})
			st.InFlight = nil
		}
	}
	d.st = st
	if err := d.persist(); err != nil {
		return err
	}
	defer func() {
		if err := d.persist(); err != nil {
			d.o.log.Warnf("direct transcript not saved at exit: %v", err)
		}
	}()

	if err := d.setupIsolation(); err != nil {
		return err
	}
	defer func() {
		if d.keepMirror(runErr) {
			return
		}
		d.closeMirror()
	}()

	return d.loop(ctx)
}

func (d *DirectAgent) waitAsk(ctx context.Context, nodeID, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		question = "The model asked a question but provided no text."
	}
	d.st.PendingAsk = question
	_ = d.persist()
	answer := askNoUserClarifyReply
	if askRequiresConsent(question) {
		answer = askNoUserConsentReply
	}
	if d.o.askHook != nil {
		d.o.log.Actionf("direct asks: %s", question)
		askStart := time.Now()
		answer = d.o.askHook(question)
		d.o.events.Record("ask", nodeID, map[string]any{
			"question":    clip(question, evReasonChars),
			"answer":      clip(answer, evReasonChars),
			"duration_ms": time.Since(askStart).Milliseconds(),
		})
	}
	if d.pauseRequested() {
		_ = d.persist()
		return "", ErrPaused
	}
	if err := ctx.Err(); err != nil {
		_ = d.persist()
		return "", err
	}
	d.st.PendingAsk = ""
	_ = d.persist()
	return answer, nil
}

func (d *DirectAgent) beginStream(id string) {
	d.o.events.Record("assistant_stream_start", d.o.tree.RootID, map[string]any{"stream_id": id})
	if s, ok := d.ctl.(directStreamSink); ok {
		s.BeginStream(id)
	}
}

func (d *DirectAgent) emitDelta(id, delta, full string) {
	d.o.events.Live("assistant_delta", d.o.tree.RootID, map[string]any{
		"stream_id": id,
		"text":      delta,
		"full":      full,
	})
	if s, ok := d.ctl.(directStreamSink); ok {
		s.AppendStream(delta)
		if full != "" {
			s.ReplaceStream(full)
		}
	}
}

func (d *DirectAgent) endStream(id, full string) {
	d.o.events.Record("assistant_stream_end", d.o.tree.RootID, map[string]any{
		"stream_id": id,
		"text":      clip(full, evReasonChars),
		"chars":     len(full),
	})
	if s, ok := d.ctl.(directStreamSink); ok {
		s.EndStream(full)
	}
}

// directStreamSink is implemented by RunHandle so the UI can read live tokens
// from Snapshot/GET. Tests may omit it.
type directStreamSink interface {
	BeginStream(id string)
	AppendStream(delta string)
	ReplaceStream(full string)
	EndStream(full string)
}

func (d *DirectAgent) prepareRoot() error {
	if err := d.o.tree.UpdateNode(d.o.tree.RootID, func(n *models.TaskNode) error {
		n.Type = models.NodeTypeLeaf
		if n.State == "" || n.State == models.TaskStatePending {
			n.State = models.TaskStateRunning
		}
		return nil
	}); err != nil {
		return err
	}
	return d.o.checkpoint()
}

func (d *DirectAgent) loop(ctx context.Context) error {
	maxSteps := d.maxSteps()
	nodeID := d.o.tree.RootID
	var native []llm.Tool
	if d.o.cfg.NativeTools {
		native = nativeToolDefs(d.toolSandbox())
	}

	if d.st.PendingApproval {
		if err := d.reviewAndMaybeMerge(ctx, d.st.Summary); err != nil {
			return d.stop(err)
		}
		if d.st.Done {
			return nil
		}
	}

	if q := strings.TrimSpace(d.st.PendingAsk); q != "" {
		answer, aerr := d.waitAsk(ctx, d.o.tree.RootID, q)
		if aerr != nil {
			return d.stop(aerr)
		}
		d.appendAskResult(d.st.PendingAskID, answer)
		d.st.PendingAskID = ""
		_ = d.persist()
	}

	if finished, summary, err := d.resumePendingActions(ctx, nodeID); err != nil {
		return d.stop(err)
	} else if finished {
		return d.finish(summary)
	}

	for step := d.st.Step + 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return d.stop(err)
		}
		if d.pauseRequested() {
			return d.stop(ErrPaused)
		}
		if err := d.drainInject(ctx); err != nil {
			return d.stop(err)
		}

		d.st.Step = step
		d.o.log.Actionf("direct step %d/%d", step, maxSteps)
		d.o.events.Record("direct_step", nodeID, map[string]any{"step": step, "max": maxSteps})

		window, dropped := directRequestWindow(d.st.Messages, d.contextBudget())
		if dropped > 0 {
			d.o.log.Infof("direct context trimmed %d earlier messages (budget %d chars); system prompt and the original request were kept, and the full transcript is on disk", dropped, d.contextBudget())
		}

		reqTimeout := d.o.cfg.RequestTimeout
		stepCtx := ctx
		var cancel context.CancelFunc
		if _, hasDeadline := ctx.Deadline(); !hasDeadline && reqTimeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, reqTimeout)
		}
		streamID := fmt.Sprintf("%s-%d", d.o.tree.ID, step)
		var streamed strings.Builder
		req := llm.Request{
			Model:       d.o.cfg.Model,
			Messages:    window,
			Tools:       native,
			Temperature: d.o.cfg.Temperature,
			MaxTokens:   d.o.cfg.MaxTokens,
			Stream:      d.o.cfg.Stream,
		}
		var view protocolView
		redact := newStreamRedactor(d.o.events.Secrets())
		if d.o.cfg.Stream {
			d.beginStream(streamID)
			req.OnToken = func(delta string) {
				if delta == "" {
					return
				}
				streamed.WriteString(delta)
				vis := view.Append(delta)
				if vis == "" {
					return
				}
				if safe := redact.Add(vis); safe != "" {
					d.emitDelta(streamID, safe, redact.Visible())
				} else if s, ok := d.ctl.(directStreamSink); ok {
					s.ReplaceStream(redact.Visible())
				}
			}
		}
		resp, err := d.o.chat(stepCtx, "direct", nodeID, req)
		if cancel != nil {
			cancel()
		}
		if d.o.cfg.Stream {
			view.Flush()
			visible := redact.Flush()
			if visible == "" {
				visible = redact.Visible()
			} else {
				// Flush may only return the leftover suffix; publish the full view.
				visible = redact.Visible()
			}
			d.endStream(streamID, visible)
		}
		if err != nil {
			if ctx.Err() != nil {
				return d.stop(ctx.Err())
			}
			if errors.Is(err, context.Canceled) {
				// Session budget tripped inside chat().
				return d.stop(err)
			}
			return d.stop(err)
		}

		actions, incompleteHint := collectActions(resp)
		if incompleteHint != "" {
			d.st.Messages = append(d.st.Messages,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Content},
				llm.Message{Role: llm.RoleUser, Content: incompleteHint},
			)
			d.st.Step-- // re-ask does not consume the tool budget
			_ = d.persist()
			continue
		}
		if len(actions) == 0 {
			d.invalidRounds++
			d.st.Messages = append(d.st.Messages,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Content},
				llm.Message{Role: llm.RoleUser, Content: invalidJSONRetryPrompt(d.invalidRounds)},
			)
			if d.invalidRounds >= maxInvalidJSONRounds {
				return d.stop(fmt.Errorf("%w: %d consecutive empty/invalid JSON replies (refusing to burn the remaining step budget)",
					ErrInvalidJSON, d.invalidRounds))
			}
			_ = d.persist()
			continue
		}
		d.invalidRounds = 0

		d.st.Messages = append(d.st.Messages, llm.Message{
			Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})
		if err := d.persist(); err != nil {
			return d.stop(err)
		}
		// Honor a Pause that arrived during the model call at this
		// boundary: do not start tools or enter manual approval.
		if d.pauseRequested() {
			d.st.PendingActions = append([]taggedAction(nil), actions...)
			_ = d.persist()
			return d.stop(ErrPaused)
		}

		finished, summary, err := d.applyActions(ctx, nodeID, actions)
		if err != nil {
			return d.stop(err)
		}
		if finished {
			return d.finish(summary)
		}
		if d.pauseRequested() {
			return d.stop(ErrPaused)
		}
	}
	return d.stop(fmt.Errorf("%w: used all %d tool calls without calling finish", ErrMaxSteps, maxSteps))
}

func (d *DirectAgent) applyActions(ctx context.Context, nodeID string, actions []taggedAction) (finished bool, summary string, err error) {
	for i, act := range actions {
		d.o.log.Actionf("direct %s %s", act.Name, previewArgs(act))
		if act.Name == tools.ToolFinish {
			// Deterministic pre-finish review, the same gate worker/solo
			// apply: unresolved conflict markers and hard-coded secrets must
			// not be accepted as a successful turn. Warnings (debug prints,
			// large diffs) are logged but do not block.
			if blocked, ok := d.finishGate(ctx); !ok {
				d.appendToolOutcome(act, blocked)
				d.skipRemaining(actions[i+1:], "Not executed: the run did not finish; finish was rejected by the review gate.")
				_ = d.persist()
				return false, "", nil
			}
			summary, _ = stringFromArgs(act.Args, "summary")
			if summary == "" {
				summary = strings.TrimSpace(act.Thought)
			}
			if summary == "" {
				summary = "finished"
			}
			d.appendToolOutcome(act, "finished: "+summary)
			d.skipRemaining(actions[i+1:], "Not executed: the model already called finish in this reply.")
			if err := d.reviewAndMaybeMerge(ctx, summary); err != nil {
				return false, "", err
			}
			if d.st.Done && strings.TrimSpace(d.st.Summary) != "" {
				summary = d.st.Summary
			}
			return true, summary, nil
		}
		if act.Name == "ask" {
			question, _ := stringFromArgs(act.Args, "question")
			d.st.PendingAskID = act.ID
			answer, aerr := d.waitAsk(ctx, nodeID, question)
			if aerr != nil {
				d.skipRemaining(actions[i+1:], "Not executed: an earlier ask in the same model reply must be answered first. The run paused or stopped before this tool started. Re-issue this tool if it is still appropriate after the user's answer.")
				_ = d.persist()
				return false, "", aerr
			}
			d.appendAskResult(act.ID, answer)
			d.st.PendingAskID = ""
			// Ask is a question, not an approval. Remaining tools in this
			// same model reply are not executed; the model must decide again.
			d.skipRemaining(actions[i+1:], "Not executed: an earlier ask in the same model reply must be answered first. Re-issue this tool if it is still appropriate after the user's answer.")
			_ = d.persist()
			return false, "", nil
		}

		if err := ctx.Err(); err != nil {
			d.skipRemaining(actions[i:], "Not executed: the run was canceled before this tool started. Do not assume it ran. Re-issue only after inspecting the workspace; do not blindly repeat a side-effecting call.")
			_ = d.persist()
			return false, "", err
		}

		d.st.InFlight = &directInFlight{ID: act.ID, Name: act.Name, Args: act.Args, StartedUnix: time.Now().Unix()}
		if err := d.persist(); err != nil {
			return false, "", err
		}
		d.o.events.Record("tool_start", nodeID, map[string]any{
			"tool": act.Name,
			"args": clip(previewArgs(act), evReasonChars),
		})
		if d.ctl != nil {
			d.ctl.SetInTool(true)
		}
		callStart := time.Now()
		out, callErr := d.toolSandbox().Call(ctx, act.Name, act.Args)
		if d.ctl != nil {
			d.ctl.SetInTool(false)
		}
		d.o.events.ToolCall(nodeID, act.Name, act.Args, out, callErr, callStart)
		if callErr != nil {
			out = "ERROR: " + callErr.Error()
			d.o.log.Warnf("%s", out)
		} else {
			d.o.log.Debugf("tool out: %s", truncate(out, 240))
		}

		if d.o.cfg.NativeTools && act.ID != "" {
			d.st.Messages = append(d.st.Messages, llm.Message{
				Role: llm.RoleTool, ToolCallID: act.ID, Name: act.Name, Content: out,
			})
		} else {
			d.st.Messages = append(d.st.Messages, llm.Message{
				Role: llm.RoleUser, Content: fmt.Sprintf("Tool %s result:\n%s", act.Name, out),
			})
		}
		d.st.InFlight = nil
		if err := d.persist(); err != nil {
			return false, "", err
		}
		// After a tool returns is a safe boundary: pause here rather than
		// cancelling the call, and leave inject for the next model step.
		if d.pauseRequested() {
			d.st.PendingActions = append([]taggedAction(nil), actions[i+1:]...)
			_ = d.persist()
			return false, "", ErrPaused
		}
		if err := ctx.Err(); err != nil {
			d.skipRemaining(actions[i+1:], "Not executed: the run was canceled before this tool started. Do not assume it ran. Re-issue only after inspecting the workspace; do not blindly repeat a side-effecting call.")
			_ = d.persist()
			return false, "", err
		}
		// A failed check in this same reply must not be followed by finish
		// (or later tools planned before the model saw the failure).
		if toolOutcomeFailed(act.Name, out, callErr) {
			// The repeated-call guard does not catch "edit, run the same
			// failing check, edit again": the calls differ, but the outcome
			// never does. Name that loop and demand an observation. The
			// prompt is appended after the skipped tool results so a native
			// tool_calls message is not interleaved with a user message.
			msg, nag := d.trackRepeatedFailure(act, out, callErr)
			d.skipRemaining(actions[i+1:], "Not executed: an earlier tool in the same model reply failed or returned a non-zero exit. Inspect that result, repair if needed, and re-issue only what is still appropriate. Do not assume this tool ran.")
			if nag {
				d.st.Messages = append(d.st.Messages, llm.Message{Role: llm.RoleUser, Content: msg})
			}
			_ = d.persist()
			return false, "", nil
		}
		if msg, looped := d.trackRepeatCall(act); looped {
			d.st.Messages = append(d.st.Messages, llm.Message{Role: llm.RoleUser, Content: msg})
			_ = d.persist()
		}
	}
	return false, "", nil
}

// trackRepeatCall interrupts a loop of identical tool calls. The call is still
// executed (it may be a read whose result was already useful), but the model is
// told that repeating it cannot change anything and must change its next action.
// Nags are capped so the intervention itself does not become the loop.
func (d *DirectAgent) trackRepeatCall(act taggedAction) (string, bool) {
	if d.repeatNags >= directRepeatNags {
		return "", false
	}
	// json.Marshal sorts map keys, so two equal argument maps produce the same
	// string; fmt's %v on a map would too, but not by contract.
	args, err := json.Marshal(act.Args)
	if err != nil {
		return "", false
	}
	key := act.Name + "\x00" + string(args)
	if key == d.repeatKey {
		d.repeatCount++
	} else {
		d.repeatKey, d.repeatCount = key, 1
	}
	if d.repeatCount < directRepeatLimit {
		return "", false
	}
	d.repeatKey, d.repeatCount = "", 0
	d.repeatNags++
	d.o.log.Warnf("direct: %s called %d times with identical arguments; interrupting", act.Name, directRepeatLimit)
	d.o.events.Record("direct_repeat_call", d.o.tree.RootID, map[string]any{
		"tool":  act.Name,
		"count": directRepeatLimit,
	})
	return directRepeatCallIntervention(act.Name, string(args), directRepeatLimit), true
}

// trackRepeatedFailure catches the loop shape the repeated-call guard misses:
// the model edits, runs the same verification command, edits again, and the
// command keeps failing the same way. The calls differ, so nothing looks
// repeated, but the outcome never changes. After directFailureLimit identical
// failures the model is told to observe before editing again — the same
// escalation that measurably beat plain "fix it again" in solo mode.
func (d *DirectAgent) trackRepeatedFailure(act taggedAction, out string, callErr error) (string, bool) {
	if act.Name != tools.ToolRunBash {
		return "", false
	}
	if !toolOutcomeFailed(act.Name, out, callErr) {
		// A passing verification means the previous failure was addressed;
		// the next failure is a new problem. A successful non-verify call
		// (read/edit) does not reset the run: edits are exactly what the
		// model does between identical failures.
		d.failKey, d.failCount = "", 0
		return "", false
	}
	if d.failNags >= directFailureNags {
		return "", false
	}
	cmd, _ := stringFromArgs(act.Args, "command")
	key := stallFingerprint(d.o.projectType(), act.Name+"\n"+cmd+"\n"+out)
	if key == "" {
		return "", false
	}
	if key == d.failKey {
		d.failCount++
	} else {
		d.failKey, d.failCount = key, 1
	}
	if d.failCount < directFailureLimit {
		return "", false
	}
	d.failCount = 0
	d.failNags++
	d.o.log.Warnf("direct: %q failed the same way %d times; demanding an observation before another edit", cmd, directFailureLimit)
	d.o.events.Record("direct_repeated_failure", d.o.tree.RootID, map[string]any{
		"command": clip(cmd, evReasonChars),
		"count":   directFailureLimit,
	})
	return directFailureEscalation(cmd, out, directFailureLimit), true
}

func directFailureEscalation(command, output string, n int) string {
	return fmt.Sprintf(`The same command has now failed the same way %d times in a row: %s

%s

Your last change did not alter the outcome. Do NOT edit the same lines again on another hunch, and do NOT weaken or delete the assertion to make it pass — that is how a previous attempt made things worse.

Change strategy:
1. State in one sentence what you believe the cause is, and what observation would confirm or refute it.
2. Observe it: print the intermediate values around the failure, add temporary debug output, or run the failing scenario in a small scratch program kept inside the workspace so the project's own packages resolve.
3. If the observation contradicts your theory, discard the theory and form a new one. Do not tune numbers until something passes.
4. Only after the observation explains the failure, change the code where the observation points.
5. Re-run the command yourself before calling finish.

Keep everything that already works; this is a diagnosis step, not a rewrite.`, n, command, clip(output, 4000))
}

func directRepeatCallIntervention(name, args string, n int) string {
	return fmt.Sprintf(`You have now called %s with identical arguments %d times in a row: %s

Repeating the call cannot produce a different result. Pick one:
- You already have what you asked for — use it. Make the edit with replace_lines or write_file, or decide the file needs no change and move on.
- The result did not tell you what you needed — change the call: another path, a narrower range, or a search for the specific symbol instead of reading the whole file.
- You are checking whether something exists — search for it; do not re-read the file to find out.

Your next action must change the workspace or advance the goal.`, name, n, clip(args, 400))
}

// finishGate is the deterministic pre-finish review over the current workspace
// (or the isolated mirror in manual-approval mode). It returns the message the
// model must see before trying again when the gate blocks. Review plumbing
// errors fail open: a broken checker must not trap a finished turn.
func (d *DirectAgent) finishGate(ctx context.Context) (string, bool) {
	gate, err := d.toolSandbox().PreFinishGate(ctx)
	if err != nil {
		d.o.log.Debugf("pre-finish review unavailable: %v", err)
		return "", true
	}
	if gate == nil {
		return "", true
	}
	d.o.events.Record("finish_gate", d.o.tree.RootID, map[string]any{
		"blocking": gate.Blocking,
		"report":   clip(gate.Report, evReasonChars),
	})
	if !gate.Blocking {
		d.o.log.Warnf("direct pre-finish warnings:\n%s", gate.Report)
		return "", true
	}
	d.o.log.Warnf("direct finish rejected by review gate")
	return "finish REJECTED by review_diff. Fix these blocking issue(s) before finishing " +
		"(unresolved conflict markers and hard-coded secrets are never accepted):\n" +
		gate.Report +
		"\nFix the code, then call finish again. Debug-print and large-diff items are warnings only, but conflict/secret items must be removed.", false
}

func (d *DirectAgent) drainInject(ctx context.Context) error {
	if d.ctl == nil {
		return ctx.Err()
	}
	text, injectID := d.ctl.TakePendingInject()
	text = strings.TrimSpace(text)
	if text == "" {
		return ctx.Err()
	}
	d.st.Messages = append(d.st.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: "User follow-up (inject at the next model step; do not restart the whole task). Honor this from now on:\n" + text,
	})
	fields := map[string]any{
		"instruction": clip(text, evReasonChars),
	}
	if injectID != "" {
		fields["inject_id"] = injectID
	}
	d.o.events.Record("user_inject", d.o.tree.RootID, fields)
	d.o.log.Actionf("direct inject applied at next model step")
	return d.persist()
}

func (d *DirectAgent) pauseRequested() bool {
	return d.ctl != nil && d.ctl.PauseRequested()
}

func (d *DirectAgent) finish(summary string) error {
	summary = d.redactText(summary)
	d.st.Done = true
	d.st.Summary = summary
	d.st.InFlight = nil
	d.st.PendingActions = nil
	if d.ctl != nil {
		d.ctl.SetSummary(summary)
	}
	_ = d.o.sched.UpdateNodeState(d.o.tree.RootID, models.TaskStateCompleted, "")
	_ = d.o.tree.UpdateNode(d.o.tree.RootID, func(n *models.TaskNode) error {
		n.ResultSummary = summary
		return nil
	})
	if err := d.persist(); err != nil {
		return err
	}
	return d.o.checkpoint()
}

func (d *DirectAgent) stop(err error) error {
	_ = d.persist()
	if errors.Is(err, ErrPaused) || errors.Is(err, context.Canceled) {
		_ = d.o.checkpoint()
		return err
	}
	msg := ""
	if err != nil {
		msg = d.redactText(err.Error())
	}
	_ = d.o.sched.UpdateNodeState(d.o.tree.RootID, models.TaskStateFailed, msg)
	_ = d.o.checkpoint()
	return err
}

func (d *DirectAgent) persist() error {
	if d.st == nil {
		return nil
	}
	return saveDirectState(d.transcriptPath(), d.st, d.secretValues()...)
}

func (d *DirectAgent) secretValues() []string {
	var out []string
	if d.o != nil {
		if d.o.events != nil {
			out = append(out, d.o.events.Secrets()...)
		}
		if k := strings.TrimSpace(d.o.cfg.APIKey); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (d *DirectAgent) redactText(s string) string {
	if d.o != nil && d.o.events != nil {
		return d.o.events.RedactString(s)
	}
	for _, sec := range d.secretValues() {
		s = scrubSecret(s, sec)
	}
	return s
}

func (d *DirectAgent) openingPrompt() string {
	var b strings.Builder
	b.WriteString("The user's request for this turn:\n")
	b.WriteString(strings.TrimSpace(d.o.cfg.Goal))
	b.WriteString("\n\nWorking directory: ")
	b.WriteString(d.o.sandbox.Root)
	if m := directProjectMap(d.o.sandbox, directMapMaxEntries, directMapMaxDepth); m != "" {
		b.WriteString("\n\nShallow project map (a bounded file listing, not a semantic index — use read_file, search_files, or find_files for real contents):\n")
		b.WriteString(m)
	}
	if st := directWorkspaceStatus(d.o.sandbox.Root); st != "" {
		b.WriteString("\n\n")
		b.WriteString(st)
	}
	b.WriteString("\n\nRead and search the project as needed, make the change, run real verification, and repair from the tool output. Call finish with a concise summary of what you actually did; that summary is shown to the user as your reply.")
	return b.String()
}

func (d *DirectAgent) followUpPrompt() string {
	var b strings.Builder
	b.WriteString("The earlier tool transcript is still in this conversation. New user request:\n")
	b.WriteString(strings.TrimSpace(d.o.cfg.Goal))
	b.WriteString("\n\nContinue from the current workspace (do not redo completed work unless the new request requires it). Use tools to inspect and verify. Call finish with a concise summary of this follow-up.")
	if m := directProjectMap(d.o.sandbox, directMapMaxEntries, directMapMaxDepth); m != "" {
		b.WriteString("\n\nCurrent shallow project map (bounded listing, not a semantic index):\n")
		b.WriteString(m)
	}
	if st := directWorkspaceStatus(d.o.sandbox.Root); st != "" {
		b.WriteString("\n\n")
		b.WriteString(st)
	}
	return b.String()
}

func (d *DirectAgent) applySandboxPolicy() {
	if d == nil || d.o == nil {
		return
	}
	d.applyPolicyToSandbox(d.o.sandbox)
	if d.sb != nil && d.sb != d.o.sandbox {
		d.applyPolicyToSandbox(d.sb)
	}
}

func (d *DirectAgent) applyPolicyToSandbox(sb *tools.Sandbox) {
	if d == nil || d.o == nil || sb == nil {
		return
	}
	tools.ApplyRuntimePolicy(sb)
	if d.o.cfg.LeafApprovalMode() == LeafApprovalManual {
		sb.RequireActionConsent = true
	}
}

func directSystemGuide(sb *tools.Sandbox, dataDir string) string {
	dyn := ""
	if sb != nil {
		dyn = sb.DynamicToolDescriptions()
	}
	var b strings.Builder
	b.WriteString(directMarker)
	b.WriteString("\n\n")
	b.WriteString("You work in one growing tool transcript for this project. Use tools to actually read, search, edit, and verify. Do not claim success without checking real tool results. Do not invent file contents.\n")
	b.WriteString("search_files and find_files are literal regex/glob matching, not a semantic index.\n")
	b.WriteString("If a user follow-up arrives mid-loop, honor it from the next step; do not restart the whole task from scratch.\n")
	b.WriteString("When the work for this turn is done, call finish with a concise, factual summary written for the user — that text is your reply. Do not call finish just to narrate a failed check; repair first.\n")
	b.WriteString(projectRulesBlock(sb, dataDir))
	b.WriteString("\n")
	if sb != nil {
		b.WriteString(sb.ToolGuide())
	} else {
		b.WriteString(tools.Descriptions())
	}
	if strings.TrimSpace(dyn) != "" {
		b.WriteString("\n")
		b.WriteString(dyn)
	}
	b.WriteString("\n\nOUTPUT a single JSON object per turn: {\"thought\":\"...\",\"action\":\"tool_name\",\"args\":{...}} or use native tool calls when they are provided. Prefer one tool per reply unless you are finishing.")
	return b.String()
}

func directSystemFor(web bool, dynamicTools string) string {
	var b strings.Builder
	b.WriteString(directMarker)
	b.WriteString("\n\n")
	b.WriteString("You work in one growing tool transcript for this project. Use tools to actually read, search, edit, and verify. Do not claim success without checking real tool results. Do not invent file contents.\n")
	b.WriteString("search_files and find_files are literal regex/glob matching, not a semantic index.\n")
	b.WriteString("If a user follow-up arrives mid-loop, honor it from the next step; do not restart the whole task from scratch.\n")
	b.WriteString("When the work for this turn is done, call finish with a concise, factual summary written for the user — that text is your reply. Do not call finish just to narrate a failed check; repair first.\n\n")
	b.WriteString(tools.Descriptions())
	if web {
		b.WriteString("\n")
		b.WriteString(tools.WebDescriptions())
	}
	if strings.TrimSpace(dynamicTools) != "" {
		b.WriteString("\n")
		b.WriteString(dynamicTools)
	}
	b.WriteString("\n\nOUTPUT a single JSON object per turn: {\"thought\":\"...\",\"action\":\"tool_name\",\"args\":{...}} or use native tool calls when they are provided. Prefer one tool per reply unless you are finishing.")
	return b.String()
}

func (d *DirectAgent) resumePendingActions(ctx context.Context, nodeID string) (finished bool, summary string, err error) {
	if d.st == nil || len(d.st.PendingActions) == 0 {
		return false, "", nil
	}
	acts := append([]taggedAction(nil), d.st.PendingActions...)
	d.st.PendingActions = nil
	_ = d.persist()
	return d.applyActions(ctx, nodeID, acts)
}

func (d *DirectAgent) appendAskResult(id, answer string) {
	content := "User answer:\n" + answer
	if d.o.cfg.NativeTools && id != "" {
		d.st.Messages = append(d.st.Messages, llm.Message{
			Role: llm.RoleTool, ToolCallID: id, Name: "ask", Content: content,
		})
		return
	}
	d.st.Messages = append(d.st.Messages, llm.Message{Role: llm.RoleUser, Content: content})
}

func (d *DirectAgent) appendToolOutcome(act taggedAction, content string) {
	if d.o.cfg.NativeTools && act.ID != "" {
		d.st.Messages = append(d.st.Messages, llm.Message{
			Role: llm.RoleTool, ToolCallID: act.ID, Name: act.Name, Content: content,
		})
		return
	}
	d.st.Messages = append(d.st.Messages, llm.Message{
		Role: llm.RoleUser, Content: fmt.Sprintf("Tool %s result:\n%s", act.Name, content),
	})
}

func (d *DirectAgent) skipRemaining(rest []taggedAction, reason string) {
	for _, act := range rest {
		d.appendToolOutcome(act, reason)
	}
}

// directRequestWindow is the model-facing slice of a direct transcript: the
// system prompt and original user request are always kept (the same contract
// as trimHistory / solo compose), then as much of a tool-group-safe tail as
// fits the character budget. Dropped turns stay on disk; a short note tells
// the model they were omitted from this request only.
func directRequestWindow(msgs []llm.Message, budget int) (window []llm.Message, dropped int) {
	if budget < 0 {
		budget = 0
	}
	head := 0
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		head = 1
		if len(msgs) > 1 && msgs[1].Role == llm.RoleUser {
			head = 2
		}
	}
	if head >= len(msgs) {
		return msgs, 0
	}
	used := 0
	for i := 0; i < head; i++ {
		used += messageChars(msgs[i])
	}
	body := msgs[head:]
	tail, dropped := tailWithinChars(body, budget-used)
	if dropped == 0 {
		out := make([]llm.Message, 0, head+len(tail))
		out = append(out, msgs[:head]...)
		out = append(out, tail...)
		return out, 0
	}
	note := llm.Message{Role: llm.RoleUser, Content: directTrimNote(dropped)}
	tail, dropped = tailWithinChars(body, budget-used-messageChars(note))
	if dropped == 0 {
		out := make([]llm.Message, 0, head+len(tail))
		out = append(out, msgs[:head]...)
		out = append(out, tail...)
		return out, 0
	}
	note.Content = directTrimNote(dropped)
	out := make([]llm.Message, 0, head+1+len(tail))
	out = append(out, msgs[:head]...)
	out = append(out, note)
	out = append(out, tail...)
	return out, dropped
}

func directTrimNote(dropped int) string {
	return fmt.Sprintf("(%d earlier transcript messages were omitted from this request to fit the context budget. The full transcript remains on disk. Continue from the recent history below; do not assume omitted tool results.)", dropped)
}

// toolOutcomeFailed reports a tool result the model must react to before
// finishing: a Call error, an ERROR-prefixed payload, or a run_bash
// non-zero exit / timeout. go test failures arrive as exit_code text, not
// as a Go error, so finish in the same reply would otherwise succeed.
func toolOutcomeFailed(name, out string, callErr error) bool {
	if callErr != nil {
		return true
	}
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "ERROR:") {
		return true
	}
	if name != tools.ToolRunBash {
		return false
	}
	if strings.Contains(out, "timed_out: true") {
		return true
	}
	var code int
	if _, err := fmt.Sscanf(trimmed, "exit_code: %d", &code); err == nil && code != 0 {
		return true
	}
	return false
}

func inferOpenToolCallID(msgs []llm.Message, name string) string {
	answered := map[string]bool{}
	var lastUnanswered, lastNamed string
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" || answered[c.ID] {
				continue
			}
			lastUnanswered = c.ID
			if name != "" && c.Function.Name == name {
				lastNamed = c.ID
			}
		}
	}
	if lastNamed != "" {
		return lastNamed
	}
	return lastUnanswered
}

func inFlightRecoveryMessage(in *directInFlight, native bool) llm.Message {
	name := "unknown"
	id := ""
	if in != nil {
		if in.Name != "" {
			name = in.Name
		}
		id = in.ID
	}
	content := fmt.Sprintf(
		"The previous %s call may already have modified the workspace; its result was not recorded because the process stopped mid-call. Inspect the workspace (read_file, list_dir, search_files) before calling that tool again — do not blindly repeat a side-effecting call.",
		name,
	)
	if native && id != "" {
		return llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: name, Content: content}
	}
	return llm.Message{Role: llm.RoleUser, Content: content}
}

func directProjectMap(sb *tools.Sandbox, maxEntries, maxDepth int) string {
	if sb == nil || strings.TrimSpace(sb.Root) == "" || maxEntries <= 0 {
		return ""
	}
	type mapEntry struct{ text, rel string }
	var entries []mapEntry
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		if len(entries) >= maxEntries || depth > maxDepth {
			return
		}
		des, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range des {
			if len(entries) >= maxEntries {
				entries = append(entries, mapEntry{text: "…(truncated)"})
				return
			}
			name := e.Name()
			if tools.IsSkippedName(name) || strings.HasPrefix(name, ".") {
				continue
			}
			child := name
			if rel != "" {
				child = filepath.ToSlash(filepath.Join(rel, name))
			}
			if e.IsDir() {
				entries = append(entries, mapEntry{text: child + "/", rel: child})
				if depth < maxDepth {
					walk(filepath.Join(dir, name), child, depth+1)
				}
				continue
			}
			// Skip obviously non-text names without opening them.
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			entries = append(entries, mapEntry{text: child, rel: child})
		}
	}
	walk(sb.Root, "", 0)
	if len(entries) == 0 {
		return ""
	}
	// The map is the onboarding view of the repo, so it should show the same
	// files the project's own ignore rules consider part of the project.
	rels := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.rel != "" {
			rels = append(rels, e.rel)
		}
	}
	ignored := sb.IgnoredPaths(rels)
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.rel != "" && ignored[e.rel] {
			continue
		}
		lines = append(lines, e.text)
	}
	return strings.Join(lines, "\n")
}

// directState is the crash-safe transcript for one conversation (or session).
type directState struct {
	Version         int                `json:"version"`
	Messages        []llm.Message      `json:"messages"`
	Step            int                `json:"step"`
	Done            bool               `json:"done"`
	Summary         string             `json:"summary,omitempty"`
	PendingAsk      string             `json:"pending_ask,omitempty"`
	PendingAskID    string             `json:"pending_ask_id,omitempty"`
	PendingApproval bool               `json:"pending_approval,omitempty"`
	PendingActions  []taggedAction     `json:"pending_actions,omitempty"`
	Mirror          *tools.MirrorState `json:"mirror,omitempty"`
	InFlight        *directInFlight    `json:"in_flight,omitempty"`
}

type directInFlight struct {
	ID          string         `json:"id,omitempty"`
	Name        string         `json:"name"`
	Args        map[string]any `json:"args,omitempty"`
	StartedUnix int64          `json:"started_unix"`
}

func loadDirectState(path, secret string) (*directState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read direct transcript: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var st directState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse direct transcript: %w", err)
	}
	return &st, nil
}

func saveDirectState(path string, st *directState, secrets ...string) error {
	if st == nil || path == "" {
		return nil
	}
	cp := *st
	cp.Messages = redactDirectMessages(st.Messages, secrets...)
	cp.Summary = scrubSecretsString(cp.Summary, secrets)
	cp.PendingAsk = scrubSecretsString(cp.PendingAsk, secrets)
	if st.InFlight != nil {
		inf := *st.InFlight
		if inf.Args != nil {
			if got, ok := scrubSecrets(cloneArgs(inf.Args), secrets).(map[string]any); ok {
				inf.Args = got
			}
		}
		cp.InFlight = &inf
	}
	if len(st.PendingActions) > 0 {
		cp.PendingActions = redactPendingActions(st.PendingActions, secrets)
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "direct_*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func redactDirectMessages(msgs []llm.Message, secrets ...string) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].Content = scrubSecretsString(m.Content, secrets)
		if len(m.ToolCalls) > 0 {
			tcs := make([]llm.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tcs[j] = tc
				tcs[j].Function.Arguments = scrubSecretsString(tc.Function.Arguments, secrets)
			}
			out[i].ToolCalls = tcs
		}
	}
	return out
}

func redactPendingActions(acts []taggedAction, secrets []string) []taggedAction {
	out := make([]taggedAction, len(acts))
	for i, a := range acts {
		out[i] = a
		if a.Args != nil {
			if got, ok := scrubSecrets(cloneArgs(a.Args), secrets).(map[string]any); ok {
				out[i].Args = got
			}
		}
		out[i].Thought = scrubSecretsString(a.Thought, secrets)
	}
	return out
}

func cloneArgs(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		out := make(map[string]any, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}

func scrubSecretsString(s string, secrets []string) string {
	for _, sec := range secrets {
		s = scrubSecret(s, sec)
	}
	return s
}

// DirectTranscriptDone reports whether a shared conversation transcript
// finished its last turn (so a follow-up should append, not resume mid-loop).
func DirectTranscriptDone(path string) (done bool, exists bool, err error) {
	st, err := loadDirectState(path, "")
	if err != nil {
		return false, false, err
	}
	if st == nil {
		return false, false, nil
	}
	return st.Done, true, nil
}
