package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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
	mu        sync.Mutex
	active    map[string]*RunHandle // session id -> handle
	busy      map[string]string     // absolute workdir -> session id (live run)
	writers   map[string]int        // short editor/hunk write leases
	exclusive map[string]string     // abs workdir -> owner of an exclusive write (rollback)

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
		writers:    make(map[string]int),
		exclusive:  make(map[string]string),
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

// HandleForWorkdir returns a non-finished run that currently owns workdir
// (including a paused one whose reservation was already released).
func (m *RunManager) HandleForWorkdir(abs string) *RunHandle {
	if m == nil || abs == "" {
		return nil
	}
	m.mu.Lock()
	var cands []*RunHandle
	if sid, ok := m.busy[abs]; ok {
		if h := m.active[sid]; h != nil {
			cands = append(cands, h)
		}
	}
	for _, h := range m.active {
		if h != nil && h.workdir == abs {
			cands = append(cands, h)
		}
	}
	m.mu.Unlock()
	for _, h := range cands {
		p := h.Phase()
		if p != PhaseDone && p != PhaseFailed {
			return h
		}
	}
	return nil
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

// Forget drops the lingering terminal handle of a session whose on-disk
// artifacts have been deleted. Non-terminal (still runnable) handles are
// rejected: files must not disappear while a run is appending to them. A
// missing handle is not an error.
func (m *RunManager) Forget(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.active[sessionID]
	if !ok {
		return nil
	}
	if !h.Phase().Terminal() {
		return fmt.Errorf("%w: %s", ErrSessionActive, sessionID)
	}
	delete(m.active, sessionID)
	return nil
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
	scrubToolSecrets(o)
	h, err := m.attach(o, abs)
	if err != nil {
		m.release(abs)
	}
	return h, err
}

// Resume continues a saved guided session through the same manager.
func (m *RunManager) Resume(cfg Config, client llm.Client, log *Logger) (*RunHandle, error) {
	// Wait for a paused prior handle BEFORE Load: Load opens the event file
	// and continues numbering from its highest seq, while a UI pause sets
	// PhasePaused optimistically (user_pause event) before serve() writes
	// its terminal checkpoint/session_end events. Loading in that window
	// would resume the counter too early and collide with the events the
	// old handle is about to append (it also closed the original
	// ErrWorkdirBusy race, since the old handle releases its slot later).
	if sid := strings.TrimSpace(cfg.SessionID); sid != "" {
		if old := m.Get(sid); old != nil && old.Phase() == PhasePaused {
			<-old.Done()
		}
	}
	// Load has no side effects outside storage and may discover the workdir
	// from the saved tree; the sandbox root it builds is already absolute.
	o, err := Load(cfg, client, log)
	if err != nil {
		return nil, err
	}
	scrubToolSecrets(o)
	abs, err := filepath.Abs(o.sandbox.Root)
	if err != nil {
		o.Close() // P2-1: Load already opened the event file
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	if wd := strings.TrimSpace(cfg.WorkDir); wd != "" && wd != "." {
		if configured, ferr := filepath.Abs(wd); ferr == nil && configured != abs {
			o.Close()
			return nil, fmt.Errorf("configured workdir %s does not match saved session workdir %s", configured, abs)
		}
	}
	if err := m.reserve(abs, o.SessionID()); err != nil {
		o.Close()
		return nil, err
	}
	h, err := m.attach(o, abs)
	if err != nil {
		m.release(abs)
	}
	return h, err
}

// scrubToolSecrets hides LLM credentials from tool subprocesses in the
// web/serve path: worker bash must not be able to `echo $OPENAI_API_KEY` or
// `printenv` and pull the key into persisted tool output (NFR-1/AC-11). CLI
// modes never go through RunManager, so they inherit the environment as
// before (NFR-3).
func scrubToolSecrets(o *Orchestrator) {
	keys := []string{"OPENAI_API_KEY", "LLM_API_KEY"}
	if k := strings.TrimSpace(o.cfg.APIKey); k != "" {
		// Custom-named exports (e.g. MY_KEY=...) still carry the live key;
		// match by value so those are hidden too.
		for _, kv := range os.Environ() {
			name, val, ok := strings.Cut(kv, "=")
			if ok && val == k {
				keys = append(keys, name)
			}
		}
	}
	o.sandbox.HideEnv(keys...)
}

// reserve marks a workdir as launching-or-live. sessionID is the resuming
// session id or startingReservation for a brand-new run.
func (m *RunManager) reserve(absWorkdir, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.busy[absWorkdir]; ok {
		return fmt.Errorf("%w: %s", ErrWorkdirBusy, m.busy[absWorkdir])
	}
	if m.writers[absWorkdir] > 0 {
		return fmt.Errorf("%w: write in progress", ErrWorkdirBusy)
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

// BeginWorkdirWrite takes a short write lease on the workdir, shared with
// Start/Resume: a live run cannot start while a write is in progress, and
// a write cannot start while a run holds the directory. Multiple writes
// may overlap (for example two hunk applies on different files) unless an
// exclusive lease is held. The caller must invoke the returned unlock.
func (m *RunManager) BeginWorkdirWrite(abs, owner string) (func(), error) {
	return m.beginWorkdirWrite(abs, owner, false)
}

// BeginWorkdirWriteExclusive is the same workdir coordination used by
// Start/hunk writes, but it refuses to overlap any other writer. Rollback
// uses this so a journaled multi-file restore is not interleaved with a
// hunk or editor write. It is not a second lock namespace.
func (m *RunManager) BeginWorkdirWriteExclusive(abs, owner string) (func(), error) {
	return m.beginWorkdirWrite(abs, owner, true)
}

func (m *RunManager) beginWorkdirWrite(abs, owner string, exclusive bool) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	if abs == "" {
		return nil, fmt.Errorf("workdir write lease requires a path")
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		owner = "write"
	}
	m.mu.Lock()
	if sid, ok := m.busy[abs]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrWorkdirBusy, sid)
	}
	if m.writers == nil {
		m.writers = make(map[string]int)
	}
	if m.exclusive == nil {
		m.exclusive = make(map[string]string)
	}
	if m.exclusive[abs] != "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: exclusive write in progress", ErrWorkdirBusy)
	}
	if exclusive && m.writers[abs] > 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: write in progress", ErrWorkdirBusy)
	}
	m.writers[abs]++
	if exclusive {
		m.exclusive[abs] = owner
	}
	held := exclusive
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if held && m.exclusive[abs] == owner {
				delete(m.exclusive, abs)
			}
			m.writers[abs]--
			if m.writers[abs] <= 0 {
				delete(m.writers, abs)
				delete(m.exclusive, abs)
			}
			m.mu.Unlock()
		})
	}, nil
}

// WorkdirBusy reports whether a run or write lease currently owns abs.
func (m *RunManager) WorkdirBusy(abs string) (string, bool) {
	if m == nil || abs == "" {
		return "", false
	}
	if resolved, err := filepath.Abs(abs); err == nil {
		abs = resolved
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sid, ok := m.busy[abs]; ok {
		return sid, true
	}
	if m.writers[abs] > 0 {
		return "write", true
	}
	return "", false
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
	if o.cfg.AgentLoopMode() == AgentLoopDirect {
		// Direct mode never enters plan review; the tool loop starts immediately.
		h.phase = PhaseRunning
		h.direct = NewDirect(o, h)
		o.askHook = h.directAskHook
		if o.cfg.LeafApprovalMode() == LeafApprovalManual {
			o.SetLeafApprovalHook(h.handleLeafApproval)
		}
	} else {
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

	h.launchCh = make(chan struct{})
	go h.serve()
	if !o.cfg.HoldServe {
		h.Launch()
	}
	return h, nil
}

// RunHandle is one live guided run plus the control surface the web layer
// drives with POSTs. All action methods are safe for concurrent use.
type RunHandle struct {
	mgr    *RunManager
	o      *Orchestrator
	g      *Guider
	direct *DirectAgent
	io     *channelGuided

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

	// Direct-mode mid-run control. pauseReq is honored at the next tool or
	// model-step boundary (a running tool is not cancelled). pendingInject
	// is drained into the same loop before the next model call.
	pauseReq        bool
	pendingInject   string
	pendingInjectID string
	injectAppliedAt time.Time
	inTool          bool
	lastSummary     string

	// Live token buffer for Snapshot/GET reconnect. Not persisted to JSONL.
	liveStreamID  string
	liveAssistant string

	// Direct-mode ask: Answer sends on askCh; Pause/Abort wake askWake or ctx.
	askCh   chan string
	askWake chan struct{}

	// eventCancel detaches the phase-tracking subscription.
	eventCancel func()

	// launchCh is closed by Launch (or immediately when HoldServe is
	// false) so serve can wait for snapshots and hooks to be installed.
	launchCh   chan struct{}
	launchOnce sync.Once

	// beforeTerminal runs after the agent loop ends and before the
	// terminal phase is published or the workdir is released.
	beforeTerminal func(RunPhase)
}

// AfterTerminalPublished, if set, is called after the terminal phase is
// visible and the workdir is released, before Done is closed. Tests use
// it to observe the first moment clients can see Done. Production code
// must leave it nil.
var AfterTerminalPublished func(*RunHandle)

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
	AgentLoop     string   `json:"agent_loop,omitempty"`
	PendingInject string   `json:"pending_inject,omitempty"`
	InTool        bool     `json:"in_tool,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	LiveAssistant string   `json:"live_assistant,omitempty"`
	LiveStreamID  string   `json:"live_stream_id,omitempty"`
}

// Snapshot returns the current externally visible state.
func (h *RunHandle) Snapshot() PhaseSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := PhaseSnapshot{
		Phase:         h.phase,
		PendingAsk:    h.pendingAsk,
		ApprovalMode:  h.o.cfg.LeafApprovalMode(),
		SessionID:     h.o.SessionID(),
		Workdir:       h.workdir,
		AgentLoop:     h.o.cfg.AgentLoopMode(),
		PendingInject: h.pendingInject,
		InTool:        h.inTool,
		Summary:       h.lastSummary,
		LiveAssistant: h.liveAssistant,
		LiveStreamID:  h.liveStreamID,
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
	s.PendingAsk = h.redactSnapshot(s.PendingAsk)
	s.PendingInject = h.redactSnapshot(s.PendingInject)
	s.Summary = h.redactSnapshot(s.Summary)
	s.LiveAssistant = h.redactSnapshot(s.LiveAssistant)
	s.Error = h.redactSnapshot(s.Error)
	return s
}

func (h *RunHandle) redactSnapshot(v string) string {
	if v == "" || h == nil {
		return v
	}
	if h.o != nil && h.o.events != nil {
		return h.o.events.RedactString(v)
	}
	if h.o != nil {
		return scrubSecret(v, h.o.cfg.APIKey)
	}
	return v
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

// Launch starts a handle created with HoldServe. It is safe to call more
// than once. A handle started without HoldServe is launched automatically.
func (h *RunHandle) Launch() {
	if h == nil {
		return
	}
	h.launchOnce.Do(func() {
		if h.launchCh != nil {
			close(h.launchCh)
		}
	})
}

// SetBeforeTerminal installs a hook that runs after the agent loop ends
// and before the terminal phase is published or the workdir is released.
func (h *RunHandle) SetBeforeTerminal(fn func(RunPhase)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.beforeTerminal = fn
	h.mu.Unlock()
}

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
	if !h.waitForLaunch() {
		h.finishUnlaunched()
		return
	}
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

	var err error
	if h.direct != nil {
		err = h.direct.Run(h.ctx)
	} else {
		err = h.g.Run(h.ctx)
	}

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

	// Persistence (conversation changes / run_finished) must run while
	// the workdir is still reserved and before the terminal phase is
	// visible. Publishing Done first let clients observe completion and
	// start a new write before the previous turn's undo snapshot landed.
	// finish() still publishes the phase and releases the workdir in one
	// critical section so a visible terminal phase is bookable again.
	term := terminalPhaseFor(err, h.aborted)
	h.mu.Lock()
	// Record the terminal error before the hook so Snapshot().Error and
	// abort detection are available, but leave the phase and workdir
	// reservation unchanged until finish().
	h.runErr = err
	before := h.beforeTerminal
	h.mu.Unlock()
	if before != nil {
		before(term)
	}
	h.mgr.finish(h, func() {
		h.runErr = err
		h.phase = term
		if h.phase != PhasePaused || h.pendingAsk == "" {
			h.pendingAsk = ""
		}
		h.liveAssistant = ""
		h.liveStreamID = ""
		h.leafApprovals = make(map[string]*pendingApproval)
	})

	h.o.Close()
	if hook := AfterTerminalPublished; hook != nil {
		hook(h)
	}
	close(h.done)
}

func terminalPhaseFor(err error, aborted bool) RunPhase {
	switch {
	case err == nil:
		return PhaseDone
	case errors.Is(err, ErrPaused):
		return PhasePaused
	case errors.Is(err, context.Canceled) && !aborted:
		return PhasePaused
	default:
		return PhaseFailed
	}
}

// waitForLaunch blocks until Launch or a cancel before the loop starts.
// It returns false if the handle was aborted without being launched.
func (h *RunHandle) waitForLaunch() bool {
	if h.launchCh == nil {
		return true
	}
	select {
	case <-h.launchCh:
		return true
	default:
	}
	select {
	case <-h.launchCh:
		return true
	case <-h.ctx.Done():
		return false
	}
}

func (h *RunHandle) finishUnlaunched() {
	err := h.ctx.Err()
	if err == nil {
		err = context.Canceled
	}
	term := terminalPhaseFor(err, h.aborted)
	h.mgr.finish(h, func() {
		h.runErr = err
		h.phase = term
		h.pendingAsk = ""
		h.liveAssistant = ""
		h.liveStreamID = ""
		h.leafApprovals = make(map[string]*pendingApproval)
	})
	if h.o != nil {
		h.o.Close()
	}
	if hook := AfterTerminalPublished; hook != nil {
		hook(h)
	}
	close(h.done)
}

// finish releases the workdir reservation and applies the terminal phase
// transition atomically with respect to reserve(). Callers must run any
// persistence hook before this so a visible terminal phase implies the
// turn record is already readable.
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
		// Do NOT derive the waiting phase from the "pending" event: the
		// orchestrator records that event immediately before invoking the
		// approval hook, so observers could see leaf_approval before the
		// review payload is registered (GET approvals/{id} then 404s).
		// handleLeafApproval sets PhaseLeafApproval atomically with the map
		// registration; the event only notifies SSE clients.
		switch phase, _ := ev["phase"].(string); phase {
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
	h.mu.Lock()
	h.askWake = make(chan struct{}, 1)
	wake := h.askWake
	h.mu.Unlock()
	select {
	case d := <-ch:
		return d, nil
	case <-wake:
		if h.PauseRequested() {
			return LeafApprovalDecision{}, ErrPaused
		}
		return LeafApprovalDecision{}, ErrPaused
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
	if p != PhaseRunning && p != PhaseAsk && p != PhaseLeafApproval {
		return fmt.Errorf("%w: pause only while running, answering a question, or reviewing changes", ErrWrongPhase)
	}
	if h.direct != nil {
		h.mu.Lock()
		h.pauseReq = true
		wake := h.askWake
		h.mu.Unlock()
		h.wakeAsk(wake)
		return nil
	}
	return h.submitAt(p, "/pause", true)
}

// AddInstruction appends a new leaf while the run executes. If a leaf is
// currently running, the addition waits until that leaf finishes.
func (h *RunHandle) AddInstruction(instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("instruction is empty")
	}
	return h.submitAt(PhaseRunning, "/add "+instruction, false)
}

// SteerNow injects a follow-up into the current turn immediately: if a leaf
// is running it is cancelled (unmerged work is dropped, completed leaves are
// kept) and the instruction is added before execution continues.
func (h *RunHandle) SteerNow(instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("instruction is empty")
	}
	if h.direct != nil {
		return h.InjectAtBoundary(instruction)
	}
	return h.submitAt(PhaseRunning, "/steer "+instruction, false)
}

// InjectAtBoundary queues a follow-up to be merged into the current direct
// loop at the next model step. A tool that is already running is not
// cancelled; the instruction waits until that call returns.
func (h *RunHandle) InjectAtBoundary(instruction string) error {
	_, err := h.InjectAtBoundaryID(instruction)
	return err
}

// InjectAtBoundaryID queues a follow-up and returns a durable request id
// that acknowledgements must match before clearing persisted Steer state.
func (h *RunHandle) InjectAtBoundaryID(instruction string) (string, error) {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return "", fmt.Errorf("instruction is empty")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase.Terminal() {
		return "", ErrRunFinished
	}
	if h.phase != PhaseRunning && h.phase != PhaseAsk {
		return "", fmt.Errorf("%w: currently %s, need %s", ErrWrongPhase, h.phase, PhaseRunning)
	}
	id := newInjectID()
	h.pendingInject = instruction
	h.pendingInjectID = id
	h.injectAppliedAt = time.Time{}
	return id, nil
}

func newInjectID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("inj_%d", time.Now().UnixNano())
	}
	return "inj_" + hex.EncodeToString(b[:])
}

// LastInjectID is the id of the most recently queued boundary inject.
func (h *RunHandle) LastInjectID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pendingInjectID
}

// ClearPendingInject drops a follow-up that has not yet been drained.
func (h *RunHandle) ClearPendingInject() {
	h.mu.Lock()
	h.pendingInject = ""
	h.pendingInjectID = ""
	h.mu.Unlock()
}

func (h *RunHandle) TakePendingInject() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.pendingInject
	id := h.pendingInjectID
	h.pendingInject = ""
	h.pendingInjectID = ""
	if s != "" {
		h.injectAppliedAt = time.Now()
	}
	return s, id
}

func (h *RunHandle) PauseRequested() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pauseReq
}

func (h *RunHandle) SetInTool(v bool) {
	h.mu.Lock()
	h.inTool = v
	h.mu.Unlock()
}

func (h *RunHandle) SetSummary(s string) {
	if h.o != nil && h.o.events != nil {
		s = h.o.events.RedactString(s)
	}
	h.mu.Lock()
	h.lastSummary = s
	h.mu.Unlock()
}

func (h *RunHandle) LastSummary() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastSummary
}

func (h *RunHandle) BeginStream(id string) {
	h.mu.Lock()
	h.liveStreamID = id
	h.liveAssistant = ""
	h.mu.Unlock()
}

func (h *RunHandle) AppendStream(delta string) {
	if delta == "" {
		return
	}
	if h.o != nil && h.o.events != nil {
		delta = h.o.events.RedactString(delta)
	}
	h.mu.Lock()
	h.liveAssistant += delta
	h.mu.Unlock()
}

func (h *RunHandle) ReplaceStream(full string) {
	if h.o != nil && h.o.events != nil {
		full = h.o.events.RedactString(full)
	}
	h.mu.Lock()
	h.liveAssistant = full
	h.mu.Unlock()
}

func (h *RunHandle) EndStream(full string) {
	if h.o != nil && h.o.events != nil {
		full = h.o.events.RedactString(full)
	}
	h.mu.Lock()
	if full != "" {
		h.liveAssistant = full
	}
	h.mu.Unlock()
}

// directAskHook parks the direct loop until Answer, timeout, pause, or cancel.
// A timeout continues the loop with a "make a reasonable assumption" reply;
// pause and cancel do not treat the wake as a user answer.
func (h *RunHandle) directAskHook(question string) string {
	timeout := time.Duration(0)
	if h.o != nil {
		timeout = h.o.cfg.AskTimeout
	}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	h.mu.Lock()
	if h.phase.Terminal() {
		h.mu.Unlock()
		return "The run is no longer active. Make a reasonable assumption and continue; do not call ask again."
	}
	h.phase = PhaseAsk
	h.pendingAsk = question
	h.askCh = make(chan string, 1)
	h.askWake = make(chan struct{}, 1)
	ch, wake := h.askCh, h.askWake
	h.mu.Unlock()
	if h.o != nil && h.o.events != nil {
		h.o.events.Record("ask_pending", "", map[string]any{"question": clip(question, evReasonChars)})
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ans := <-ch:
		return ans
	case <-timer.C:
		if askRequiresConsent(question) {
			// Timeout is not consent. Pause and keep the question so a
			// resume re-surfaces it; the model never receives a yes.
			h.mu.Lock()
			h.pauseReq = true
			h.mu.Unlock()
			if h.o != nil && h.o.events != nil {
				h.o.events.Record("ask_timeout", "", map[string]any{
					"question": clip(question, evReasonChars),
					"risk":     "consent",
					"paused":   true,
				})
			}
			return ""
		}
		h.mu.Lock()
		if h.phase == PhaseAsk {
			h.phase = PhaseRunning
			h.pendingAsk = ""
		}
		h.mu.Unlock()
		if h.o != nil && h.o.events != nil {
			h.o.events.Record("ask_timeout", "", map[string]any{
				"question": clip(question, evReasonChars),
				"risk":     "clarify",
				"paused":   false,
			})
		}
		return askTimeoutClarifyReply
	case <-wake:
		return ""
	case <-h.ctx.Done():
		return ""
	}
}

func (h *RunHandle) wakeAsk(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (h *RunHandle) PendingInject() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pendingInject
}

func (h *RunHandle) InTool() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inTool
}

func (h *RunHandle) AgentLoop() string {
	if h == nil || h.o == nil {
		return AgentLoopGuided
	}
	return h.o.cfg.AgentLoopMode()
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
	if h.direct != nil {
		h.mu.Lock()
		if h.phase.Terminal() {
			h.mu.Unlock()
			return ErrRunFinished
		}
		if h.phase != PhaseAsk {
			h.mu.Unlock()
			return fmt.Errorf("%w: currently %s, need %s", ErrWrongPhase, h.phase, PhaseAsk)
		}
		ch := h.askCh
		h.phase = PhaseRunning
		h.pendingAsk = ""
		h.mu.Unlock()
		if ch == nil {
			return fmt.Errorf("%w: no pending question", ErrWrongPhase)
		}
		select {
		case ch <- text:
			return nil
		default:
			return fmt.Errorf("%w: no pending question", ErrWrongPhase)
		}
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
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("%w: leaf %s is not awaiting approval", ErrWrongPhase, nodeID)
	}
	if !approved && comment == "" {
		h.mu.Unlock()
		return fmt.Errorf("rejection comment is required")
	}
	// Leave the waiting phase before signaling the parked hook: the hook's
	// cleanup deletes the map entry on wake, so leaving the phase change to
	// that (or to the applyEvent path) opens a window where phase still
	// reads leaf_approval while GET approvals/{id} already 404s. Mirrors
	// Answer(), which flips back to running together with the submission.
	h.phase = PhaseRunning
	h.mu.Unlock()
	return h.sendLeafDecision(pa, LeafApprovalDecision{Approved: approved, Comment: comment})
}

// DecideLeafPatched is DecideLeaf plus optional hunk patches applied to the
// isolated mirror after approval and before MergeBack.
func (h *RunHandle) DecideLeafPatched(nodeID string, approved bool, comment string, patches map[string]string, deletes []string) error {
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
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("%w: leaf %s is not awaiting approval", ErrWrongPhase, nodeID)
	}
	if !approved && comment == "" {
		h.mu.Unlock()
		return fmt.Errorf("rejection comment is required")
	}
	h.phase = PhaseRunning
	h.mu.Unlock()
	return h.sendLeafDecision(pa, LeafApprovalDecision{
		Approved: approved, Comment: comment, Patches: patches, Deletes: deletes,
	})
}

func (h *RunHandle) sendLeafDecision(pa *pendingApproval, d LeafApprovalDecision) error {
	select {
	case pa.ch <- d:
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
	if p != want && (!allowAsk || p != PhaseAsk) {
		return fmt.Errorf("%w: currently %s, need %s", ErrWrongPhase, p, want)
	}
	return h.io.Submit(h.ctx, line)
}
