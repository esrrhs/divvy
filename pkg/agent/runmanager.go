package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// RunPhase is the externally visible state of a managed run. It drives which
// control actions are currently valid; the SSE/UI renders against it.
type RunPhase string

const (
	PhasePlanning     RunPhase = "planning"      // building/revising the plan
	PhasePlanReview   RunPhase = "plan_review"   // waiting for approve/adjust/abort
	PhaseRunning      RunPhase = "running"       // leaves executing
	PhaseLeafApproval RunPhase = "leaf_approval" // waiting for a leaf diff decision
	PhaseAsk          RunPhase = "ask"           // waiting for a worker question answer
	PhasePaused       RunPhase = "paused"        // saved, not running (resumable)
	PhaseDone         RunPhase = "done"          // completed successfully
	PhaseFailed       RunPhase = "failed"        // aborted or errored
)

// Sentinel errors so the HTTP layer can map them to status codes instead of
// parsing message text.
var (
	// ErrWorkdirBusy: another live run already owns the workspace directory.
	ErrWorkdirBusy = errors.New("workdir already has a running session")
	// ErrSessionActive: the session id is already managed/live.
	ErrSessionActive = errors.New("session is already running")
	// ErrRunFinished: a control action reached a run that already ended.
	ErrRunFinished = errors.New("run has already finished")
	// ErrWrongPhase: the action is not valid in the run's current phase.
	ErrWrongPhase = errors.New("action not valid in current phase")
)

// RunManager owns the live runs reachable from the web/desktop layer: at most
// one run per workspace directory, indexed by session id. Finished runs stay
// registered (in a terminal phase) until the manager is shut down; historical
// sessions that never ran here are read straight from storage.
type RunManager struct {
	mu     sync.Mutex
	active map[string]*RunHandle // session id -> handle
	busy   map[string]string     // absolute workdir -> session id

	// baseCtx is cancelled by Shutdown (process signal). Every handle's run
	// context derives from it, so Ctrl+C unwinds and checkpoints all runs.
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

func NewRunManager() *RunManager {
	return NewRunManagerWithContext(context.Background())
}

// NewRunManagerWithContext ties run lifetimes to ctx: cancelling it (as the
// serve process does on SIGINT/SIGTERM) interrupts every live run.
func NewRunManagerWithContext(ctx context.Context) *RunManager {
	baseCtx, cancel := context.WithCancel(ctx)
	return &RunManager{
		active:     make(map[string]*RunHandle),
		busy:       make(map[string]string),
		baseCtx:    baseCtx,
		baseCancel: cancel,
	}
}

// Shutdown interrupts every live run (each persists its tree first, so all
// of them are resumable), then waits up to grace for their goroutines to
// finish. Safe to call multiple times.
func (m *RunManager) Shutdown(grace time.Duration) {
	m.baseCancel()
	m.mu.Lock()
	handles := make([]*RunHandle, 0, len(m.active))
	for _, h := range m.active {
		handles = append(handles, h)
	}
	m.mu.Unlock()

	deadline := time.Now().Add(grace)
	for _, h := range handles {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case <-h.Done():
		case <-time.After(remaining):
			return
		}
	}
}

// Get returns a live handle for a session id, or nil.
func (m *RunManager) Get(sessionID string) *RunHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[sessionID]
}

// ActiveSessions lists live session ids.
func (m *RunManager) ActiveSessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}
	return ids
}

// startingReservation marks a workdir between Start's lock acquisition and
// the orchestrator knowing its minted session id. It is never a valid id.
const startingReservation = ":starting:"

// Start creates a goal-driven guided run and launches it. The workdir lock is
// reserved before the orchestrator is built so two concurrent starts can
// never both win the same workspace.
func (m *RunManager) Start(cfg Config, client llm.Client, log *Logger) (*RunHandle, error) {
	if strings.TrimSpace(cfg.Goal) == "" {
		return nil, fmt.Errorf("goal is required")
	}
	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}

	if err := m.reserve(abs, startingReservation); err != nil {
		return nil, err
	}
	o, err := NewFromGoal(cfg, client, log)
	if err != nil {
		m.release(abs)
		return nil, err
	}
	h, err := m.attach(o, abs)
	if err != nil {
		m.release(abs)
	}
	return h, err
}

// Resume continues a saved guided session through the same manager.
func (m *RunManager) Resume(cfg Config, client llm.Client, log *Logger) (*RunHandle, error) {
	// Load has no side effects outside storage and may discover the workdir
	// from the saved tree; the sandbox root it builds is already absolute.
	o, err := Load(cfg, client, log)
	if err != nil {
		return nil, err
	}
	// A UI pause publishes PhasePaused optimistically (from the user_pause
	// event) before serve() has finished unwinding the worker and releasing
	// the workdir. A resume posted in that window would otherwise lose the
	// race with ErrWorkdirBusy; wait for the prior handle to fully settle.
	if old := m.Get(o.SessionID()); old != nil && old.Phase() == PhasePaused {
		<-old.Done()
	}
	abs, err := filepath.Abs(o.sandbox.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	if wd := strings.TrimSpace(cfg.WorkDir); wd != "" && wd != "." {
		if configured, ferr := filepath.Abs(wd); ferr == nil && configured != abs {
			return nil, fmt.Errorf("configured workdir %s does not match saved session workdir %s", configured, abs)
		}
	}
	if err := m.reserve(abs, o.SessionID()); err != nil {
		return nil, err
	}
	h, err := m.attach(o, abs)
	if err != nil {
		m.release(abs)
	}
	return h, err
}

// reserve marks a workdir as launching-or-live. sessionID is the resuming
// session id or startingReservation for a brand-new run.
func (m *RunManager) reserve(absWorkdir, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.busy[absWorkdir]; ok {
		return fmt.Errorf("%w: %s", ErrWorkdirBusy, m.busy[absWorkdir])
	}
	if sessionID != startingReservation {
		if h, ok := m.active[sessionID]; ok && !h.Phase().Terminal() {
			return fmt.Errorf("%w: %s", ErrSessionActive, sessionID)
		}
	}
	m.busy[absWorkdir] = sessionID
	return nil
}

func (m *RunManager) release(absWorkdir string) {
	m.mu.Lock()
	delete(m.busy, absWorkdir)
	m.mu.Unlock()
}

// attach wires orchestrator + channel IO into a handle and launches it. On a
// registration collision (another Start won the session/workdir race) the
// freshly built orchestrator is closed and the error returned.
func (m *RunManager) attach(o *Orchestrator, absWorkdir string) (*RunHandle, error) {
	ctx, cancel := context.WithCancel(m.baseCtx)
	h := &RunHandle{
		mgr:           m,
		o:             o,
		workdir:       absWorkdir,
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		leafApprovals: make(map[string]*pendingApproval),
	}
	h.phase = PhasePlanning
	if done, _, _ := o.tree.GetLeafProgress(); done > 0 {
		// Resumed mid-execution: Guider skips planning/review.
		h.phase = PhaseRunning
	}

	io := newChannelGuided(nil)
	g := NewGuider(o, io)
	g.askNotice = h.onWorkerQuestion
	h.io = io
	h.g = g

	if o.cfg.LeafApprovalMode() == LeafApprovalManual {
		o.SetLeafApprovalHook(h.handleLeafApproval)
	}

	m.mu.Lock()
	if old, exists := m.active[o.SessionID()]; exists && !old.Phase().Terminal() {
		m.mu.Unlock()
		cancel()
		o.Close()
		m.release(absWorkdir)
		return nil, fmt.Errorf("%w: %s", ErrSessionActive, o.SessionID())
	}
	m.active[o.SessionID()] = h
	m.busy[absWorkdir] = o.SessionID()
	m.mu.Unlock()

	go h.serve()
	return h, nil
}

// RunHandle is one live guided run plus the control surface the web layer
// drives with POSTs. All action methods are safe for concurrent use.
type RunHandle struct {
	mgr *RunManager
	o   *Orchestrator
	g   *Guider
	io  *channelGuided

	workdir string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	phase      RunPhase
	pendingAsk string
	runErr     error
	// aborted marks a cancel requested by the user's Abort action, distinct
	// from a process-level interrupt: an aborted run reports failed, while
	// SIGINT/Shutdown reports paused (stopped but resumable).
	aborted       bool
	leafApprovals map[string]*pendingApproval

	// eventCancel detaches the phase-tracking subscription.
	eventCancel func()
}

// pendingApproval is one leaf parked after verify, before merge: the review
// request (node, title, file changes for the diff UI) and the decision
// channel the worker blocks on.
type pendingApproval struct {
	req LeafApprovalRequest
	ch  chan LeafApprovalDecision
}

// PhaseSnapshot is a point-in-time view for API responses.
type PhaseSnapshot struct {
	Phase         RunPhase `json:"phase"`
	PendingAsk    string   `json:"pending_ask,omitempty"`
	LeafApproving string   `json:"leaf_approving,omitempty"`
	ApprovalMode  string   `json:"approval_mode"`
	SessionID     string   `json:"session_id"`
	Workdir       string   `json:"workdir"`
	Error         string   `json:"error,omitempty"`
}

// Snapshot returns the current externally visible state.
func (h *RunHandle) Snapshot() PhaseSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := PhaseSnapshot{
		Phase:        h.phase,
		PendingAsk:   h.pendingAsk,
		ApprovalMode: h.o.cfg.LeafApprovalMode(),
		SessionID:    h.o.SessionID(),
		Workdir:      h.workdir,
	}
	// Serially executed leaves park at most one approval; pick the smallest
	// id deterministically without re-locking (Snapshot already holds h.mu).
	for id := range h.leafApprovals {
		if s.LeafApproving == "" || id < s.LeafApproving {
			s.LeafApproving = id
		}
	}
	if h.runErr != nil {
		s.Error = h.runErr.Error()
	}
	return s
}

// Phase returns just the run phase.
func (h *RunHandle) Phase() RunPhase {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.phase
}

// Terminal reports whether the run can no longer accept actions.
func (p RunPhase) Terminal() bool {
	return p == PhasePaused || p == PhaseDone || p == PhaseFailed
}

// Wait blocks until the run ends and returns its terminal error (nil on
// success / clean pause).
func (h *RunHandle) Wait() error {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runErr
}

// Done returns the completion channel.
func (h *RunHandle) Done() <-chan struct{} { return h.done }

// SessionID and Workdir identify the run.
func (h *RunHandle) SessionID() string { return h.o.SessionID() }
func (h *RunHandle) Workdir() string   { return h.workdir }

// Orchestrator exposes the live tree/events for SSE and reads.
func (h *RunHandle) Orchestrator() *Orchestrator { return h.o }

// SubscribeEvents fans out subsequent recorder events to the web layer. The
// returned cancel detaches the subscriber and closes its channel.
func (h *RunHandle) SubscribeEvents() (<-chan map[string]any, func()) {
	return h.o.events.Subscribe()
}

// SecretValues lists in-memory-only credentials (e.g. a UI-supplied API key)
// that must never cross to an SSE consumer or disk unmasked.
func (h *RunHandle) SecretValues() []string {
	if k := strings.TrimSpace(h.o.cfg.APIKey); k != "" {
		return []string{k}
	}
	return nil
}

// serve runs the guided lifecycle and records the terminal phase.
func (h *RunHandle) serve() {
	events, cancelSub := h.o.events.Subscribe()
	h.mu.Lock()
	h.eventCancel = cancelSub
	h.mu.Unlock()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for ev := range events {
			h.applyEvent(ev)
		}
	}()

	err := h.g.Run(h.ctx)

	cancelSub()
	<-eventsDone

	// An interrupted run (Ctrl+C / manager Shutdown / user abort while a
	// leaf runs) must persist its tree: the guider only checkpoints at some
	// cancellation points, and a cancel arriving mid-leaf would otherwise
	// leave only stale state on disk. Close() (below) needs the sinks open,
	// so checkpoint before it.
	if errors.Is(err, context.Canceled) {
		_ = h.o.checkpoint()
	}

	// Free the workdir slot and publish the terminal phase in one critical
	// section: phase is what waiters (UI/SSE, tests) poll, so a terminal
	// phase must imply the workdir is bookable again. Releasing after the
	// phase left a window where a paused run looked resumable but Resume
	// still got ErrWorkdirBusy. Lock order is manager -> handle; no handle
	// method ever takes the manager lock, so the nesting is deadlock-free.
	h.mgr.finish(h, func() {
		h.runErr = err
		switch {
		case err == nil:
			h.phase = PhaseDone
		case errors.Is(err, ErrPaused):
			h.phase = PhasePaused
		case errors.Is(err, context.Canceled) && !h.aborted:
			// Process-level interrupt: stopped but fully resumable.
			h.phase = PhasePaused
		default:
			h.phase = PhaseFailed
		}
		h.pendingAsk = ""
		h.leafApprovals = make(map[string]*pendingApproval)
	})

	h.o.Close()
	close(h.done)
}

// finish releases the workdir reservation and applies the terminal phase
// transition atomically with respect to reserve().
func (m *RunManager) finish(h *RunHandle, setPhase func()) {
	m.mu.Lock()
	delete(m.busy, h.workdir)
	h.mu.Lock()
	setPhase()
	h.mu.Unlock()
	m.mu.Unlock()
}

// applyEvent tracks the phase from the persisted event stream, so the UI
// state machine is derived from the same trace SSE subscribers see.
func (h *RunHandle) applyEvent(ev map[string]any) {
	kind, _ := ev["kind"].(string)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase.Terminal() {
		return
	}
	switch kind {
	case "plan_review":
		switch phase, _ := ev["phase"].(string); phase {
		case "pending":
			h.phase = PhasePlanReview
		case "":
			// Older decision events carry "decision" instead of "phase".
			switch d, _ := ev["decision"].(string); d {
			case "approved":
				h.phase = PhaseRunning
			case "aborted":
				// Leave as-is; serve() records the terminal failure.
			default:
				h.phase = PhasePlanReview // adjusted: another round
			}
		case "approved":
			h.phase = PhaseRunning
		case "rejected", "adjusted":
			h.phase = PhasePlanReview
		}
	case "leaf_approval":
		switch phase, _ := ev["phase"].(string); phase {
		case "pending":
			h.phase = PhaseLeafApproval
		case "approved", "rejected":
			h.phase = PhaseRunning
		}
	case "state_change":
		// Only actual leaf execution marks the run as running. Decomposition
		// also happens during planning and after an adjust/replan; treating
		// DECOMPOSING as execution would wrongly move a parked plan review
		// (and a post-adjustment replan) into the running phase.
		if to, _ := ev["to"].(string); to == string(models.TaskStateRunning) ||
			to == string(models.TaskStateVerifying) {
			if h.phase == PhasePlanReview || h.phase == PhasePlanning {
				h.phase = PhaseRunning
			}
		}
	case "user_pause":
		h.phase = PhasePaused
	}
}

// onWorkerQuestion is installed as Guider.askNotice: the worker blocks until
// an Answer action submits a line.
func (h *RunHandle) onWorkerQuestion(question string) {
	h.mu.Lock()
	h.phase = PhaseAsk
	h.pendingAsk = question
	h.mu.Unlock()
	if h.o != nil && h.o.events != nil {
		h.o.events.Record("ask_pending", "", map[string]any{"question": clip(question, evReasonChars)})
	}
}

// handleLeafApproval parks the verified leaf until DecideLeaf posts the
// verdict; cancelling the run (abort/shutdown) unblocks it with ctx.Err.
func (h *RunHandle) handleLeafApproval(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error) {
	ch := make(chan LeafApprovalDecision, 1)
	h.mu.Lock()
	h.leafApprovals[req.NodeID] = &pendingApproval{req: req, ch: ch}
	// Set the phase under the same lock as the registration: a client that
	// observes leaf_approval (via the phase API) must always find the review
	// payload in the map. The recorder's leaf_approval event can fan out to
	// phase tracking before this hook body runs, so the event alone is racy.
	if !h.phase.Terminal() {
		h.phase = PhaseLeafApproval
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.leafApprovals, req.NodeID)
		h.mu.Unlock()
	}()
	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		return LeafApprovalDecision{}, ctx.Err()
	}
}

// ---- Plan review actions -------------------------------------------------

// ApprovePlan releases a run parked at the plan review.
func (h *RunHandle) ApprovePlan() error {
	return h.submitAt(PhasePlanReview, "/approve", false)
}

// AdjustPlan sends free-form feedback that replans, keeping the run in review.
func (h *RunHandle) AdjustPlan(feedback string) error {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return fmt.Errorf("adjust feedback is empty")
	}
	return h.submitAt(PhasePlanReview, feedback, false)
}

// Abort stops the run: during review it is a soft /abort (plan saved, not
// executed); anywhere else live it cancels the context so leaves unwind and
// the last checkpoint remains resumable.
func (h *RunHandle) Abort() error {
	h.mu.Lock()
	p := h.phase
	h.mu.Unlock()
	if p.Terminal() {
		return ErrRunFinished
	}
	if p == PhasePlanReview {
		return h.submitAt(PhasePlanReview, "/abort", false)
	}
	h.mu.Lock()
	h.aborted = true
	h.mu.Unlock()
	h.cancel()
	return nil
}

// Pause saves and stops a running (or question-waiting) run.
func (h *RunHandle) Pause() error {
	h.mu.Lock()
	p := h.phase
	h.mu.Unlock()
	if p.Terminal() {
		return ErrRunFinished
	}
	if p != PhaseRunning && p != PhaseAsk {
		return fmt.Errorf("%w: pause only while running or answering a question", ErrWrongPhase)
	}
	return h.submitAt(p, "/pause", true)
}

// AddInstruction appends a new leaf while the run executes.
func (h *RunHandle) AddInstruction(instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("instruction is empty")
	}
	return h.submitAt(PhaseRunning, "/add "+instruction, false)
}

// Redo resets a node to PENDING so the scheduler runs it again.
func (h *RunHandle) Redo(nodeID string) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return fmt.Errorf("node id is empty")
	}
	return h.submitAt(PhaseRunning, "/redo "+nodeID, false)
}

// Answer replies to a pending worker question.
func (h *RunHandle) Answer(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("answer is empty")
	}
	if err := h.submitAt(PhaseAsk, text, true); err != nil {
		return err
	}
	h.mu.Lock()
	h.phase = PhaseRunning
	h.pendingAsk = ""
	h.mu.Unlock()
	return nil
}

// DecideLeaf posts the human verdict for a leaf parked in manual approval.
// A rejection requires a non-empty comment that is fed back to the retry.
func (h *RunHandle) DecideLeaf(nodeID string, approved bool, comment string) error {
	comment = strings.TrimSpace(comment)
	h.mu.Lock()
	if h.phase.Terminal() {
		h.mu.Unlock()
		return ErrRunFinished
	}
	if h.phase != PhaseLeafApproval {
		h.mu.Unlock()
		return fmt.Errorf("%w: no leaf is awaiting approval", ErrWrongPhase)
	}
	pa, ok := h.leafApprovals[nodeID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: leaf %s is not awaiting approval", ErrWrongPhase, nodeID)
	}
	if !approved && comment == "" {
		return fmt.Errorf("rejection comment is required")
	}
	select {
	case pa.ch <- LeafApprovalDecision{Approved: approved, Comment: comment}:
		return nil
	case <-h.ctx.Done():
		return ErrRunFinished
	}
}

// PendingApproval returns the diff review request for one leaf currently
// parked before MergeBack. The web layer serves it as the approval payload.
func (h *RunHandle) PendingApproval(nodeID string) (LeafApprovalRequest, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pa, ok := h.leafApprovals[nodeID]
	if !ok {
		return LeafApprovalRequest{}, false
	}
	return pa.req, true
}

// PendingApprovalNodeIDs lists leaves currently awaiting a diff decision
// (normally one, since the guider executes leaves serially).
func (h *RunHandle) PendingApprovalNodeIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.leafApprovals))
	for id := range h.leafApprovals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// submitAt delivers a line only while the run is in the expected phase.
// allowAsk lets /pause through while a question is pending (that is the
// terminal Guider's own behavior).
func (h *RunHandle) submitAt(want RunPhase, line string, allowAsk bool) error {
	h.mu.Lock()
	p := h.phase
	h.mu.Unlock()
	if p.Terminal() {
		return ErrRunFinished
	}
	if p != want && !(allowAsk && p == PhaseAsk) {
		return fmt.Errorf("%w: currently %s, need %s", ErrWrongPhase, p, want)
	}
	return h.io.Submit(h.ctx, line)
}
