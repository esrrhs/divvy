package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Orchestrator is the top-level decompose → execute → verify loop.
type Orchestrator struct {
	cfg     Config
	tree    *engine.TaskTree
	sched   *engine.Scheduler
	storage *engine.Storage
	llm     llm.Client
	sandbox *tools.Sandbox
	log     *Logger
	usage   *UsageTracker
	pricing *cost.Pricing
	budget  *budgetGuard
	cpMu    sync.Mutex
	mergeMu sync.Mutex

	// leafStart records when each leaf first started executing, so the
	// MaxElapsed budget spans all of that leaf's attempts rather than
	// restarting on every retry.
	leafMu    sync.Mutex
	leafStart map[string]time.Time

	// Session trace sinks: the human-readable log file and the JSONL event stream.
	logFile *os.File
	events  *EventRecorder

	// treeBC coalesces and publishes tree_snapshot events to subscribers.
	treeBC *treeBroadcaster

	// askHook, when set, lets a leaf worker ask the user a question mid-run
	// and block on the answer. Nil in batch mode; the guided flow sets it.
	askHook func(question string) string

	// leafApproval, when set, pauses verified leaves for human diff review
	// (manual approval mode). Nil means the configured mode degrades to
	// auto-approve with a warning.
	leafApproval LeafApprovalHook

	// wave is the plan → run → aggregate loop's bookkeeping (see wave.go).
	wave waveState

	// planning counts in-flight decompositions. The wave throttle reads it to
	// avoid a second expansion in the same pass: a decomposition in progress
	// has not grown the tree yet, so pending-leaf count underestimates the
	// frontier and the cap would leak.
	planning atomic.Int32
}

// SetLeafApprovalHook installs the human review gate used in manual mode.
func (o *Orchestrator) SetLeafApprovalHook(h LeafApprovalHook) {
	o.leafApproval = h
}

// New creates an orchestrator around an existing tree.
func New(cfg Config, tree *engine.TaskTree, client llm.Client, log *Logger) (*Orchestrator, error) {
	if log == nil {
		log = NewLogger(cfg.Verbose)
	}
	sandbox, err := tools.NewSandbox(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	if cfg.WebEnabled {
		webClient, werr := tools.NewWebClient(cfg.SearchURL, cfg.RequestTimeout)
		if werr != nil {
			return nil, werr
		}
		sandbox.Web = webClient
	}
	if cfg.BrowserEnabled {
		browserClient, berr := tools.NewBrowserClient()
		if berr != nil {
			return nil, berr
		}
		sandbox.Browser = browserClient
	}
	if cfg.GitCommit && !tools.IsRepo(sandbox.Root) {
		return nil, fmt.Errorf("-git-commit requires %s to be a git repository", sandbox.Root)
	}
	if cfg.LeafApprovalMode() == LeafApprovalManual && !cfg.Isolate {
		return nil, fmt.Errorf("manual leaf approval requires isolate mode (-isolate): rejected changes must be discardable")
	}
	storage, err := engine.NewStorage(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	pricing, err := loadPricing(cfg.PricingJSON)
	if err != nil {
		return nil, err
	}
	tree.WorkDir = sandbox.Root
	tree.Goal = cfg.Goal
	tree.ModelName = cfg.Model
	tree.PriceFor = pricing.PriceFor
	o := &Orchestrator{
		cfg:       cfg,
		tree:      tree,
		sched:     engine.NewScheduler(tree),
		storage:   storage,
		llm:       client,
		sandbox:   sandbox,
		log:       log,
		usage:     NewUsageTracker(),
		pricing:   pricing,
		leafStart: make(map[string]time.Time),
	}
	o.openSinks()
	return o, nil
}

// openSinks wires the session log file and JSONL event stream under the data
// directory. Sink problems degrade gracefully: a run must not fail just
// because its trace cannot be written.
func (o *Orchestrator) openSinks() {
	logDir := filepath.Join(o.cfg.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err == nil {
		logPath := filepath.Join(logDir, o.tree.ID+".log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			o.logFile = f
			o.log.Tee(f)
		} else {
			o.log.Warnf("session log not writable (%s): %v", logPath, err)
		}
	} else {
		o.log.Warnf("session log directory not creatable (%s): %v", logDir, err)
	}

	evPath := filepath.Join(o.cfg.DataDir, "events", o.tree.ID+".jsonl")
	if rec, err := NewEventRecorder(evPath); err == nil {
		o.events = rec
		// Scrub the live credential from any event field before it is
		// persisted (tool output previews, messages): the on-disk secrecy
		// invariant must not depend on every call site remembering to mask.
		rec.AddSecret(o.cfg.APIKey)
		o.treeBC = newTreeBroadcaster(o)
		o.treeBC.start()
	} else {
		o.log.Warnf("event stream not writable (%s): %v", evPath, err)
	}

	o.sched.OnStateChange = func(id string, old, new models.TaskState, msg string) {
		f := map[string]any{"from": string(old), "to": string(new)}
		if strings.TrimSpace(msg) != "" {
			f["error"] = clip(msg, evReasonChars)
		}
		o.events.Record("state_change", id, f)
		if o.treeBC != nil {
			o.treeBC.mark("state")
		}
	}
}

// LogPath and EventPath report the on-disk trace locations.
func (o *Orchestrator) LogPath() string {
	return filepath.Join(o.cfg.DataDir, "logs", o.tree.ID+".log")
}

func (o *Orchestrator) EventPath() string {
	return filepath.Join(o.cfg.DataDir, "events", o.tree.ID+".jsonl")
}

// Close releases trace files and the shared browser process.
func (o *Orchestrator) Close() {
	if o.treeBC != nil {
		o.treeBC.close()
	}
	if o.events != nil {
		_ = o.events.Close()
	}
	if o.logFile != nil {
		_ = o.logFile.Close()
	}
	if o.sandbox != nil && o.sandbox.Browser != nil {
		o.sandbox.Browser.Close()
	}
}

// NewFromGoal starts a fresh session.
func NewFromGoal(cfg Config, client llm.Client, log *Logger) (*Orchestrator, error) {
	if cfg.SessionID == "" {
		cfg.SessionID = "sess_" + time.Now().Format("20060102_150405")
	}
	if cfg.Goal == "" {
		return nil, fmt.Errorf("goal is required")
	}
	tree := engine.NewTaskTree(cfg.SessionID, cfg.Goal, cfg.Goal)
	o, err := New(cfg, tree, client, log)
	if err != nil {
		return nil, err
	}
	_ = o.tree.UpdateNode(tree.RootID, func(n *models.TaskNode) error {
		n.MaxRetries = cfg.MaxRetries
		return nil
	})
	return o, nil
}

// Load resumes a saved session.
func Load(cfg Config, client llm.Client, log *Logger) (*Orchestrator, error) {
	o, err := loadTree(cfg, client, log, false)
	if err != nil {
		return nil, err
	}
	o.resetInterrupted()
	return o, nil
}

// LoadSnapshot loads a saved session for inspection without the resume-side
// repair. Read-only views (-status, -report) must show what was saved, not what
// a resume would turn it into: resetInterrupted revives a FAILED leaf as
// PENDING so it gets another attempt, which is wrong for a session that has
// already ended and is only being looked at. A solo run is exactly that shape —
// its whole tree is one leaf, so every failed solo session read back as
// "PENDING / interrupted" while the report's own outcome line said "failed".
//
// It also trusts the saved workdir over the shell's, so `-report` run from
// somewhere else describes the directory the session actually ran in.
func LoadSnapshot(cfg Config, client llm.Client, log *Logger) (*Orchestrator, error) {
	return loadTree(cfg, client, log, true)
}

func loadTree(cfg Config, client llm.Client, log *Logger, preferSavedWorkDir bool) (*Orchestrator, error) {
	storage, err := engine.NewStorage(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.SessionID == "" {
		latest, err := os.ReadFile(filepath.Join(cfg.DataDir, "LATEST"))
		if err != nil {
			return nil, fmt.Errorf("no session specified and no LATEST pointer: %w", err)
		}
		cfg.SessionID = string(bytesTrim(latest))
	}
	tree, err := storage.LoadTree(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	if cfg.Goal == "" {
		cfg.Goal = tree.Goal
	}
	if tree.WorkDir != "" && (preferSavedWorkDir || cfg.WorkDir == ".") {
		cfg.WorkDir = tree.WorkDir
	}
	return New(cfg, tree, client, log)
}

func bytesTrim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func (o *Orchestrator) parkInterrupted(id string) {
	reason := ""
	if live, ok := o.tree.CloneNode(id); !ok || strings.TrimSpace(live.ErrorMsg) == "" {
		reason = "interrupted"
	}
	_ = o.sched.UpdateNodeState(id, models.TaskStatePending, reason)
}

func (o *Orchestrator) resetInterrupted() {
	ids := o.tree.NodeIDsInStates(
		models.TaskStateRunning,
		models.TaskStateDecomposing,
		models.TaskStateVerifying,
		models.TaskStateFailed,
	)
	for _, id := range ids {
		live, ok := o.tree.CloneNode(id)
		if !ok {
			continue
		}
		// A failed leaf blocks every task that depends on it and the run
		// then exits. Resume puts it back in the queue with a fresh stall
		// window so the same fingerprint does not immediately stop it again.
		if live.State == models.TaskStateFailed && live.Type == models.NodeTypeLeaf {
			_ = o.tree.UpdateNode(id, func(n *models.TaskNode) error {
				n.ErrorHistory = nil
				return nil
			})
		}
		if live.State == models.TaskStateFailed && live.Type != models.NodeTypeLeaf {
			continue
		}
		reason := ""
		if strings.TrimSpace(live.ErrorMsg) == "" {
			reason = "interrupted; will retry"
		}
		_ = o.sched.UpdateNodeState(id, models.TaskStatePending, reason)
	}
}

// SessionID returns the tree session id.
func (o *Orchestrator) SessionID() string { return o.tree.ID }

// Tree returns the live task tree.
func (o *Orchestrator) Tree() *engine.TaskTree { return o.tree }

// StorageDir is the persistence directory.
func (o *Orchestrator) StorageDir() string { return o.cfg.DataDir }

// parallel returns the effective number of concurrent leaf workers.
func (o *Orchestrator) parallel() int {
	if o.cfg.Parallel > 1 {
		return o.cfg.Parallel
	}
	return 1
}

// Run drives the engine until the root completes, fails, or the context is
// cancelled. Ready leaves run concurrently, bounded by cfg.Parallel.
func (o *Orchestrator) Run(ctx context.Context) (runErr error) {
	runStart := time.Now()
	o.recordSessionStart("run")
	defer func() { o.recordSessionEnd("run", runStart, runErr) }()

	ctx = o.startBudget(ctx)
	o.log.Banner("divvy")
	o.log.Infof("session %s", o.tree.ID)
	o.log.Infof("model   %s", o.cfg.Model)
	o.log.Infof("workdir %s", o.sandbox.Root)
	o.log.Infof("goal    %s", o.cfg.Goal)
	o.log.Infof("parallel %d", o.parallel())
	if saved := o.tree.TotalTokenUsage(); saved.Calls > 0 {
		o.log.Infof("tokens so far: %d (%d calls)", saved.TotalTokens, saved.Calls)
	}
	// Workspace consistency is reported before any leaf runs: an alias or a
	// module/tree mismatch makes one file addressable two ways, and a leaf
	// working inside an isolated mirror cannot see the workspace well enough to
	// notice. Left unreported it surfaces as a merge conflict or as a leaf
	// guessing at a data problem it did not create.
	for _, w := range o.layoutWarnings() {
		o.log.Warnf("layout check: %s", w)
	}
	o.printTree()

	if err := o.checkpoint(); err != nil {
		return err
	}

	// Surface plan-quality warnings during a normal run, not just under -plan.
	// A resumed session already has its tree, so this reports immediately; a
	// fresh session reports once decomposition has produced leaves.
	warned := map[string]bool{}
	reportPlanWarnings := func() {
		for _, w := range o.planWarnings() {
			if warned[w] {
				continue
			}
			warned[w] = true
			o.log.Warnf("plan check: %s", w)
			o.events.Record("plan_warning", "", map[string]any{
				"warning": clip(w, evReasonChars),
			})
		}
	}
	reportPlanWarnings()

	// A resumed tree may have been planned before structural completeness was
	// enforced (or by a planner that ignored the instruction). Heal it before
	// any leaf runs: a goal-level acceptance with nothing to route its failure
	// to is a dead end that -resume can never escape.
	o.healPlanStructure()

	// Where does the workspace stand before any leaf runs? Knowing this keeps a
	// project that was already broken from being blamed on the first wave.
	if o.waveEnabled() {
		o.baselineAggregate(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(o.aggregateCommands()) == 0 || !o.aggregateReady() {
			o.log.Infof("wave loop: nothing to aggregate yet; waves are throttled, but the build is " +
				"only checked once the project has code (use -aggregate to force a command)")
		}
	}

	parallel := o.parallel()
	done := make(chan string, 64)
	var mu sync.Mutex
	inFlight := make(map[string]bool, parallel)
	// outputClaims tracks the workspace-relative output files held by
	// in-flight leaves. Concurrent leaves writing the same file race, and the
	// loser silently loses verified work, so a ready leaf whose declared
	// outputs are already claimed waits for the claim to be released.
	outputClaims := map[string]bool{}
	var wg sync.WaitGroup
	var firstErr error

	// tryDispatch launches fn. It reports (started, poolFull): started is false
	// when id is already in flight, when the pool is full, or when one of the
	// leaf's claimed outputs is already held by another in-flight leaf.
	// claims (may be nil) are reserved for the duration of fn. The claim test
	// and the reservation happen under one lock hold so two leaves that both
	// want the same file can never both be admitted.
	tryDispatch := func(id string, claims map[string]bool, fn func(context.Context) error) (started, poolFull bool) {
		mu.Lock()
		if inFlight[id] {
			mu.Unlock()
			return false, false
		}
		if len(inFlight) >= parallel {
			mu.Unlock()
			return false, true
		}
		for c := range claims {
			if outputClaims[c] {
				mu.Unlock()
				return false, false
			}
		}
		inFlight[id] = true
		for c := range claims {
			outputClaims[c] = true
		}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fn(ctx)
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				o.log.Errorf("%s: %v", id, err)
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
			mu.Lock()
			delete(inFlight, id)
			for c := range claims {
				delete(outputClaims, c)
			}
			mu.Unlock()
			select {
			case done <- id:
			default:
			}
		}()
		return true, false
	}

	idle := 0
	for {
		if err := ctx.Err(); err != nil {
			wg.Wait()
			o.logUsage()
			_ = o.checkpoint()
			return err
		}
		mu.Lock()
		err := firstErr
		busy := len(inFlight)
		claims := make(map[string]bool, len(outputClaims))
		for k := range outputClaims {
			claims[k] = true
		}
		mu.Unlock()

		// Wave boundary: everything planned has landed. Check the workspace as a
		// whole before planning more — this is what makes the run a loop instead
		// of a single up-front decomposition followed by one late assembly.
		//
		// It runs before goal acceptance on purpose. Acceptance is the expensive,
		// end-to-end claim, and spending it on a workspace that does not even
		// build wastes it: an aggregate failure also has a clear owner (the
		// repair leaf), where an acceptance failure after a hundred leaves
		// belongs to nobody in particular.
		if busy == 0 && err == nil && o.waveBoundary() {
			if err := o.runWaveAggregate(ctx); err != nil {
				wg.Wait()
				o.logUsage()
				_ = o.checkpoint()
				return err
			}
			continue
		}

		// Goal-level acceptance: when every child of a compound root is done,
		// run the end-to-end DoD before the root is allowed to complete.
		if o.sched.RootNeedsAcceptance() {
			if err := o.verifyRootAcceptance(ctx); err != nil {
				if ctx.Err() != nil {
					wg.Wait()
					o.logUsage()
					_ = o.checkpoint()
					return ctx.Err()
				}
				// Persist the terminal (failed) tree for the non-ctx path too.
				_ = o.checkpoint()
				return err
			}
			_ = o.checkpoint()
			o.printTree()
			continue
		}
		if o.sched.IsComplete() {
			wg.Wait()
			o.printTree()
			o.logUsage()
			o.log.Okf("root task completed")
			return o.checkpoint()
		}
		if o.sched.HasFailed() {
			wg.Wait()
			o.printTree()
			o.logUsage()
			// The failed node states live only in memory until checkpointed;
			// without this save the session would reopen as PENDING and be
			// misclassified as resumable instead of failed.
			_ = o.checkpoint()
			msg := ""
			if root, ok := o.tree.CloneNode(o.tree.RootID); ok {
				msg = root.ErrorMsg
			}
			return fmt.Errorf("root task failed: %s", msg)
		}
		if err != nil && busy == 0 {
			_ = o.checkpoint()
			return err
		}

		progressed := false
		ready := o.sched.GetReadyLeafNodesAvoiding(claims)

		// Decomposition fills the tree, but only while the wave has room: the
		// frontier is capped at -wave leaves so the planner has to look at real
		// code before it thinks about more of the goal.
		if o.waveHasRoom(len(ready) == 0) {
			if node := o.sched.GetNextDecomposableNode(); node != nil {
				n := node
				if started, _ := tryDispatch(n.ID, nil, func(c context.Context) error {
					o.planning.Add(1)
					defer o.planning.Add(-1)
					if derr := o.decompose(c, n); derr != nil {
						if c.Err() != nil {
							o.parkInterrupted(n.ID)
						} else {
							o.log.Errorf("decompose %s: %v", n.ID, derr)
							_ = o.sched.UpdateNodeState(n.ID, models.TaskStateFailed, derr.Error())
						}
					}
					return nil
				}); started {
					progressed = true
				}
				// New leaves may carry weak contracts; surface that before they run.
				reportPlanWarnings()
				// A decomposition can free ready leaves; recompute rather than
				// running a list taken before the tree grew.
				ready = o.sched.GetReadyLeafNodesAvoiding(claims)
			}
		}
		// Leaves fill the pool, but only up to this wave's quota. A wave that
		// launches everything the planner emitted is the old behaviour again:
		// a planner that writes five leaves in one shot would run all five
		// before the first aggregate, which is exactly the "assemble at the
		// end" shape this loop exists to avoid.
		budget := o.waveBudget()
		for _, leaf := range ready {
			if budget <= 0 {
				break
			}
			lf := leaf
			leafClaims := engine.OutputSetClaims(lf.Contract.Outputs)
			started, poolFull := tryDispatch(lf.ID, leafClaims, func(c context.Context) error {
				if lerr := o.executeLeaf(c, lf); lerr != nil {
					return lerr
				}
				// Only landed work counts towards a wave: a failed leaf is
				// handled by the retry/failure path, not by aggregation.
				o.wave.noteLeaf()
				return nil
			})
			if poolFull {
				break
			}
			if started {
				progressed = true
				o.wave.dispatched.Add(1)
				budget--
			}
		}

		if !progressed {
			mu.Lock()
			busy = len(inFlight)
			mu.Unlock()
			// busy == 0 also means no output is claimed (claims are taken and
			// released under this same mutex), so no leaf is being held back by
			// a claim here — a claim-blocked leaf always implies busy > 0, and
			// the wait below releases it.
			if busy == 0 {
				idle++
				if idle > 3 {
					wg.Wait()
					o.logUsage()
					return fmt.Errorf("no runnable tasks (%s)", o.sched.DescribeStuck())
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			idle = 0
			select {
			case <-done:
			case <-ctx.Done():
			}
			_ = o.checkpoint()
			o.printTree()
			continue
		}
		idle = 0
	}
}

// leafLayoutDrift returns a concrete failure message when the leaf sandbox
// violates the layout/path contract (module nesting, extra go.mod, outputs
// landed under the wrong prefix). Empty string means the layout is acceptable.
func (o *Orchestrator) leafLayoutDrift(root string, leaf *models.TaskNode) string {
	outputs := leaf.Contract.Outputs
	issues, err := tools.CheckLeafLayout(root, outputs)
	if err != nil {
		o.log.Debugf("leaf layout check: %v", err)
		return ""
	}
	if len(issues) == 0 {
		return ""
	}
	return tools.FormatLayoutIssues(issues)
}

// layoutWarnings formats workspace-level inconsistencies that make a path mean
// more than one thing (symlink aliases, module path vs. directory tree).
//
// These are deliberately not blockers: a symlink can be perfectly legitimate
// in a real project, and the workspace belongs to the user, not to divvy. They
// are reported once, up front, so the problem is visible before a leaf spends
// its budget rediscovering it — and -strict turns them into a failure for
// unattended runs that should not start on a workspace that needs fixing.
func (o *Orchestrator) layoutWarnings() []string {
	issues, err := tools.CheckLayout(o.sandbox.Root)
	if err != nil {
		o.log.Debugf("layout check: %v", err)
		return nil
	}
	if len(issues) == 0 {
		return nil
	}
	out := make([]string, 0, len(issues))
	paths := make([]string, 0, len(issues))
	for _, is := range issues {
		paths = append(paths, is.Path)
		out = append(out, fmt.Sprintf("%s (%s): %s", is.Path, is.Kind, is.Detail))
	}
	o.events.Record("layout_warning", "", map[string]any{
		"count": len(issues),
		"paths": clip(strings.Join(paths, ", "), evReasonChars),
	})
	return out
}

// RunPlan decomposes the goal into a full tree of leaves without executing any
// of them. The saved session can later be executed with -resume.
func (o *Orchestrator) RunPlan(ctx context.Context) (runErr error) {
	runStart := time.Now()
	o.recordSessionStart("plan")
	defer func() { o.recordSessionEnd("plan", runStart, runErr) }()

	ctx = o.startBudget(ctx)
	o.log.Banner("divvy (plan)")
	o.log.Infof("session %s", o.tree.ID)
	o.log.Infof("model   %s", o.cfg.Model)
	o.log.Infof("workdir %s", o.sandbox.Root)
	o.log.Infof("goal    %s", o.cfg.Goal)

	if err := o.checkpoint(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = o.checkpoint()
			return err
		}
		node := o.sched.GetNextDecomposableNode()
		if node == nil {
			break
		}
		o.log.Actionf("plan: decompose %s — %s", node.ID, node.Title)
		if err := o.decompose(ctx, node); err != nil {
			if ctx.Err() != nil {
				o.parkInterrupted(node.ID)
				_ = o.checkpoint()
				return err
			}
			o.log.Errorf("decompose %s: %v", node.ID, err)
			_ = o.sched.UpdateNodeState(node.ID, models.TaskStateFailed, err.Error())
			_ = o.checkpoint()
			return fmt.Errorf("plan failed at %s: %w", node.ID, err)
		}
		_ = o.checkpoint()
		o.printTree()
	}

	warns := o.planWarnings()
	for _, w := range warns {
		o.log.Warnf("plan check: %s", w)
	}
	// Workspace layout is reported next to the plan check because both answer
	// "is it safe to start executing?" — one about the tree, one about the
	// ground it runs on.
	layoutWarns := o.layoutWarnings()
	for _, w := range layoutWarns {
		o.log.Warnf("layout check: %s", w)
	}
	totalWarns := len(warns) + len(layoutWarns)

	o.printTree()
	_, total, _ := o.tree.GetLeafProgress()
	o.logUsage()
	if totalWarns > 0 {
		o.log.Warnf("plan ready: %d leaf tasks, %d warning(s) (execute with: -resume -session %s)",
			total, totalWarns, o.tree.ID)
	} else {
		o.log.Okf("plan ready: %d leaf tasks, no issues (execute with: -resume -session %s)",
			total, o.tree.ID)
	}
	if err := o.checkpoint(); err != nil {
		return err
	}
	if o.cfg.Strict && totalWarns > 0 {
		return fmt.Errorf("plan check failed: %d warning(s)", totalWarns)
	}
	return nil
}

// buildPlan decomposes every pending node until the tree is fully planned.
// It is the interactive equivalent of the decomposition half of RunPlan.
func (o *Orchestrator) buildPlan(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			_ = o.checkpoint()
			return err
		}
		node := o.sched.GetNextDecomposableNode()
		if node == nil {
			return nil
		}
		o.log.Actionf("plan: decompose %s — %s", node.ID, node.Title)
		if err := o.decompose(ctx, node); err != nil {
			if ctx.Err() != nil {
				o.parkInterrupted(node.ID)
				_ = o.checkpoint()
				return ctx.Err()
			}
			_ = o.sched.UpdateNodeState(node.ID, models.TaskStateFailed, err.Error())
			_ = o.checkpoint()
			return fmt.Errorf("planning failed at %s: %w", node.ID, err)
		}
		_ = o.checkpoint()
	}
}

// replan discards the root's current children and re-decomposes with the
// user's adjustment feedback incorporated into the root description.
func (o *Orchestrator) replan(ctx context.Context, feedback string) error {
	if err := o.tree.ResetChildren(o.tree.RootID); err != nil {
		return err
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.Description = fmt.Sprintf("%s\n[User plan adjustment — the new plan MUST reflect this]: %s",
			strings.TrimSpace(n.Description), strings.TrimSpace(feedback))
		return nil
	}); err != nil {
		return err
	}
	return o.buildPlan(ctx)
}

// chat wraps o.llm.Chat and records token usage under a call kind and node.
func (o *Orchestrator) chat(ctx context.Context, kind, nodeID string, req llm.Request) (*llm.Response, error) {
	start := time.Now()
	resp, err := o.llm.Chat(ctx, req)
	o.events.LLMCall(kind, nodeID, req, resp, start, err)
	if resp == nil {
		return resp, err
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	o.usage.Add(kind, resp.Usage)
	if nodeID != "" {
		_ = o.tree.UpdateNode(nodeID, func(n *models.TaskNode) error {
			n.TokenUsage.Calls++
			n.TokenUsage.PromptTokens += resp.Usage.PromptTokens
			n.TokenUsage.CompletionTokens += resp.Usage.CompletionTokens
			n.TokenUsage.TotalTokens += resp.Usage.TotalTokens
			return nil
		})
	}
	if err == nil && o.budget != nil && o.budget.check() {
		return resp, context.Canceled
	}
	return resp, err
}

func (o *Orchestrator) logUsage() {
	u, calls := o.usage.Total()
	saved := o.tree.TotalTokenUsage()
	if calls == 0 && saved.Calls == 0 {
		return
	}
	if calls > 0 {
		o.log.Infof("llm usage (this run): %d calls, %s", calls, o.usageText(u))
	}
	if saved.Calls > 0 {
		su := llm.Usage{
			PromptTokens:     saved.PromptTokens,
			CompletionTokens: saved.CompletionTokens,
			TotalTokens:      saved.TotalTokens,
		}
		o.log.Infof("llm usage (session total): %d calls, %s", saved.Calls, o.usageText(su))
	}
	if !o.cfg.Verbose {
		return
	}
	keys, kinds := o.usage.SortedKinds()
	for _, k := range keys {
		o.log.Debugf("  %s: %s", k, o.formatKindUsage(kinds[k]))
	}
}

// usageText renders "prompt P + completion C = T tokens" plus estimated cost
// when a price is known for the configured model.
func (o *Orchestrator) usageText(u llm.Usage) string {
	s := fmt.Sprintf("prompt %d + completion %d = %d tokens",
		u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	if c, ok := o.costLabel(u); ok {
		s += ", est. cost " + c
	}
	return s
}

// formatKindUsage renders one per-kind usage line, including estimated cost
// when the model is priced.
func (o *Orchestrator) formatKindUsage(u llm.Usage) string {
	s := fmt.Sprintf("%d prompt + %d completion = %d tokens",
		u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	if c, ok := o.costLabel(u); ok {
		s += ", est. " + c
	}
	return s
}

// costLabel resolves the configured model's price and formats the USD cost
// of u. Every usage/status view uses this so cost is reported identically.
func (o *Orchestrator) costLabel(u llm.Usage) (string, bool) {
	price, ok := o.pricing.PriceFor(o.cfg.Model)
	if !ok {
		return "", false
	}
	return cost.FormatUSD(price.Cost(u.PromptTokens, u.CompletionTokens)), true
}

func (o *Orchestrator) executeLeaf(ctx context.Context, leaf *models.TaskNode) error {
	// In isolated mode the leaf works in a mirror of the workspace; changes
	// reach the real workspace only after verification passes. Retries of the
	// same leaf reuse the mirror so a partial attempt can iterate on its error.
	var mirror *tools.Mirror
	sb := o.sandbox
	if o.cfg.Isolate {
		var err error
		mirror, sb, err = o.newLeafMirror()
		if err != nil {
			return err
		}
		// Close over the variable, not the pointer captured at defer time:
		// a merge-conflict re-snapshot replaces mirror below, and the fresh
		// mirror must be the one closed on exit.
		defer func() {
			if mirror != nil {
				mirror.Close()
			}
		}()
	}

	prevErr := ""
	for attempt := 1; ; attempt++ {
		live, ok := o.tree.CloneNode(leaf.ID)
		if !ok {
			return fmt.Errorf("leaf %s disappeared", leaf.ID)
		}
		leaf = live

		// Stamp the first attempt only: the wall-clock budget must span every
		// retry of this leaf, not restart on each one.
		o.leafMu.Lock()
		if _, seen := o.leafStart[leaf.ID]; !seen {
			o.leafStart[leaf.ID] = time.Now()
		}
		o.leafMu.Unlock()

		o.log.Infof("execute %s [attempt %d]: %s", leaf.ID, attempt, leaf.Title)
		if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStateRunning, ""); err != nil {
			return err
		}

		summary, err := o.runWorker(ctx, sb, leaf, prevErr)
		if err != nil {
			if ctx.Err() != nil {
				o.parkInterrupted(leaf.ID)
				return ctx.Err()
			}
			prevErr = err.Error()
			o.log.Warnf("worker error: %s", prevErr)
			// Consecutive empty/invalid JSON is a soft abort: the model produced
			// no usable tool calls, so charging MaxRetries / RetryCount would
			// burn the leaf's budget on syntax noise instead of real verify
			// failures. Deadline still applies so a permanently broken model
			// cannot spin forever.
			if errors.Is(err, ErrInvalidJSON) {
				if softErr := o.softRetryInvalidJSON(ctx, leaf, prevErr); softErr != nil {
					return softErr
				}
				if !retryable(o, leaf.ID) {
					return nil
				}
				attempt-- // undo the for-loop increment so MaxRetries is not charged
				continue
			}
			o.recordNodeError(leaf.ID, prevErr)
			if err := o.retryOrGiveUp(ctx, leaf, attempt, prevErr); err != nil {
				return err
			}
			if !retryable(o, leaf.ID) {
				return nil
			}
			continue
		}

		if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStateVerifying, ""); err != nil {
			return err
		}
		vr := o.verify(ctx, sb, leaf)
		if ctx.Err() != nil {
			o.parkInterrupted(leaf.ID)
			return ctx.Err()
		}
		if vr.OK {
			// Layout fail-fast: soft DoD greps can pass against a drifted path
			// (module-named nesting, extra go.mod). Refuse the leaf with a
			// concrete found-vs-expected message instead of merging and
			// forcing later leaves to burn tokens on broken imports.
			if drift := o.leafLayoutDrift(sb.Root, leaf); drift != "" {
				prevErr = drift
				o.recordNodeError(leaf.ID, prevErr)
				o.log.Warnf("layout drift for %s (attempt %d): %s", leaf.ID, attempt, truncate(drift, 240))
				o.events.Record("layout_drift", leaf.ID, map[string]any{
					"error": clip(drift, evReasonChars),
				})
				if err := o.retryOrGiveUp(ctx, leaf, attempt, prevErr); err != nil {
					return err
				}
				if !retryable(o, leaf.ID) {
					return nil
				}
				continue
			}
			// Manual human review: pause with the leaf's diff BEFORE any merge.
			// Requires an isolated mirror so a rejection can discard the whole
			// attempt and re-run against a fresh snapshot.
			if o.cfg.LeafApprovalMode() == LeafApprovalManual && mirror != nil {
				if o.leafApproval == nil {
					o.log.Warnf("manual leaf approval configured but no hook installed; auto-approving %s", leaf.ID)
				} else {
					changes, perr := mirror.PreviewChanges()
					if perr != nil {
						return fmt.Errorf("preview changes for review: %w", perr)
					}
					o.events.Record("leaf_approval", leaf.ID, map[string]any{"phase": "pending", "files": len(changes)})
					decision, aerr := o.leafApproval(ctx, LeafApprovalRequest{
						NodeID: leaf.ID, Title: leaf.Title, Changes: changes,
					})
					if aerr != nil {
						if ctx.Err() != nil {
							o.parkInterrupted(leaf.ID)
							_ = o.checkpoint()
							return ctx.Err()
						}
						return aerr
					}
					if !decision.Approved {
						o.events.Record("leaf_approval", leaf.ID, map[string]any{
							"phase": "rejected", "comment": clip(decision.Comment, evReasonChars),
						})
						o.recordNodeError(leaf.ID, "changes rejected by reviewer: "+decision.Comment)
						mirror.Close()
						mirror = nil // until the fresh snapshot exists
						fresh, fsb, ferr := o.newLeafMirror()
						if ferr != nil {
							return ferr
						}
						mirror, sb = fresh, fsb
						prevErr = "Your changes were REVIEWED AND REJECTED by the human reviewer and have been discarded. " +
							"Reviewer comment: " + strings.TrimSpace(decision.Comment) +
							"\nRe-read the task, redo the work in the fresh isolated workspace, then finish again."
						o.log.Warnf("leaf %s rejected by reviewer; re-snapshotting", leaf.ID)
						if err := o.retryOrGiveUp(ctx, leaf, attempt, prevErr); err != nil {
							return err
						}
						if !retryable(o, leaf.ID) {
							return nil
						}
						continue
					}
					o.events.Record("leaf_approval", leaf.ID, map[string]any{"phase": "approved"})
				}
			}
			if mirror != nil || o.cfg.GitCommit {
				var merr error
				summary, merr = o.publishLeaf(mirror, leaf, summary)
				if merr != nil {
					prevErr = merr.Error()
					o.recordNodeError(leaf.ID, prevErr)
					o.log.Warnf("publish failed for %s (attempt %d): %v", leaf.ID, attempt, merr)
					// A layout problem (the destination resolves through a
					// symlink) is not a sibling race: a fresh snapshot resolves
					// the same link the same way, and the leaf cannot fix it by
					// trying again. Fail with the structural reason instead of
					// burning the retry budget on an unwinnable loop.
					var layout *tools.MirrorLayoutError
					if errors.As(merr, &layout) {
						o.log.Errorf("%s cannot be merged, workspace layout is inconsistent", leaf.ID)
						if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStateFailed, layout.Error()); err != nil {
							return err
						}
						return nil
					}
					// A merge conflict means a sibling leaf committed a change
					// to the same files after this mirror was taken. Retrying
					// against the same mirror would hit the identical
					// conflict forever, so re-snapshot: the fresh mirror
					// already contains the sibling's merged work, and the
					// retried leaf can reconcile against it.
					var conflict *tools.MergeConflictError
					if errors.As(merr, &conflict) {
						mirror.Close()
						mirror = nil // until the fresh snapshot exists; closes on the ferr return too
						fresh, fsb, ferr := o.newLeafMirror()
						if ferr != nil {
							return ferr
						}
						mirror, sb = fresh, fsb
						prevErr = merr.Error() + "\n\nA sibling leaf already changed those file(s) and its version was kept. " +
							"Re-read them, reconcile your change with what is now there, and re-apply only your part."
						o.log.Warnf("re-snapshotting %s after merge conflict on %s", leaf.ID, strings.Join(conflict.Paths, ", "))
					}
					if err := o.retryOrGiveUp(ctx, leaf, attempt, prevErr); err != nil {
						return err
					}
					if !retryable(o, leaf.ID) {
						return nil
					}
					continue
				}
			}
			_ = o.tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
				n.ResultSummary = summary
				n.ErrorMsg = ""
				return nil
			})
			if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStateCompleted, ""); err != nil {
				return err
			}
			o.log.Okf("completed %s — %s", leaf.ID, truncate(summary, 120))
			return nil
		}

		prevErr = vr.Output
		o.recordNodeError(leaf.ID, vr.Output)
		o.log.Warnf("verification failed for %s (attempt %d)", leaf.ID, attempt)
		if err := o.retryOrGiveUp(ctx, leaf, attempt, vr.Output); err != nil {
			return err
		}
		if !retryable(o, leaf.ID) {
			return nil
		}
	}
}

// newLeafMirror snapshots the shared workspace into a fresh isolated mirror
// and returns a sandbox confined to it, carrying over the read-only
// capabilities (web/browser) that are not workspace-scoped.
func (o *Orchestrator) newLeafMirror() (*tools.Mirror, *tools.Sandbox, error) {
	mirror, err := tools.NewMirror(o.sandbox.Root)
	if err != nil {
		return nil, nil, err
	}
	sb, err := mirror.Sandbox()
	if err != nil {
		mirror.Close()
		return nil, nil, err
	}
	sb.Web = o.sandbox.Web
	sb.Browser = o.sandbox.Browser
	sb.HideEnv(o.sandbox.HiddenEnv()...)
	o.log.Infof("isolated workspace: %s", tools.TrimPath(sb.Root))
	return mirror, sb, nil
}

// softRetryInvalidJSON parks the leaf for another try without charging
// RetryCount or the MaxRetries budget. Invalid/empty JSON early-stops are
// syntax noise from a weak model, not a real verify failure — burning retries
// on them exhausts the leaf before it ever gets to run tools. The wall-clock
// deadline still applies so a permanently broken model cannot spin forever.
func (o *Orchestrator) softRetryInvalidJSON(ctx context.Context, leaf *models.TaskNode, errMsg string) error {
	if o.deadlineExceeded(leaf.ID, 1) {
		return o.giveUpOnDeadline(leaf, errMsg)
	}
	_ = o.tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
		n.ErrorMsg = errMsg
		return nil
	})
	if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStatePending, errMsg); err != nil {
		return err
	}
	_ = o.checkpoint()
	delay := RetryDelay(1, o.cfg.retryMin(), o.cfg.retryMax())
	o.log.Warnf("soft-retry %s after invalid JSON (not charged) in %s", leaf.ID, delay)
	o.events.Record("invalid_json_soft_retry", leaf.ID, map[string]any{
		"delay_ms": delay.Milliseconds(),
		"reason":   clip(errMsg, evReasonChars),
	})
	if err := waitBackoff(ctx, delay); err != nil {
		o.parkInterrupted(leaf.ID)
		return err
	}
	return nil
}

// retryable reports whether the leaf should keep retrying inside this worker:
// only if it is still a pending leaf. A node that gave up (FAILED) or was
// re-split into a compound goes back to the main loop instead.
func retryable(o *Orchestrator, id string) bool {
	live, ok := o.tree.CloneNode(id)
	return ok && live.Type == models.NodeTypeLeaf && live.State == models.TaskStatePending
}

// publishLeaf folds a verified leaf's mirror into the shared workspace and,
// when GitCommit is on, records one git commit — atomically with respect to
// other leaves so each commit contains exactly its own leaf's changes.
// Commit problems are logged but not fatal: verification already passed.
func (o *Orchestrator) publishLeaf(mirror *tools.Mirror, leaf *models.TaskNode, summary string) (string, error) {
	o.mergeMu.Lock()
	defer o.mergeMu.Unlock()

	if mirror != nil {
		merged, deleted, err := mirror.MergeBack()
		if err != nil {
			return summary, err
		}
		if len(merged) > 0 {
			summary += "\nMerged files: " + strings.Join(merged, ", ")
			o.log.Okf("merged %d file(s) from %s", len(merged), leaf.ID)
		}
		for _, d := range deleted {
			o.log.Actionf("merged deletion of %s from %s", d, leaf.ID)
		}
	}

	if o.cfg.GitCommit {
		msg := fmt.Sprintf("leaf(%s): %s\n\n%s", leaf.ID, leaf.Title, firstLine(summary))
		committed, hash, err := tools.CommitAll(o.sandbox.Root, msg)
		if err != nil {
			o.log.Errorf("git commit for %s: %v", leaf.ID, err)
		} else if committed {
			summary += "\nCommit: " + hash
			o.log.Okf("committed %s as %s", leaf.ID, hash)
		}
	}
	return summary, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func (o *Orchestrator) retryOrGiveUp(ctx context.Context, leaf *models.TaskNode, attempt int, errMsg string) error {
	// The wall-clock guard is the backstop for MaxRetries=0: a leaf whose DoD
	// can never pass would otherwise retry forever, spending tokens on a goal
	// that cannot land. It is checked before scheduling more work, and the
	// node is failed (not left pending) so the run terminates with a clear
	// reason instead of spinning.
	if o.deadlineExceeded(leaf.ID, attempt) {
		return o.giveUpOnDeadline(leaf, errMsg)
	}
	// Quality gate: a leaf that keeps producing the same failure is looping,
	// not progressing. Stop feeding it attempts — with MaxRetries=0 (the
	// default) nothing else would ever end this loop.
	if run, isStalled := o.stalled(leaf.ID); isStalled {
		return o.handleStall(leaf, run, errMsg)
	}
	if o.cfg.MaxRetries > 0 && attempt >= o.cfg.MaxRetries {
		return o.handleLeafFailure(leaf, errMsg)
	}
	if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStatePending, errMsg); err != nil {
		return err
	}
	_ = o.checkpoint()
	delay := RetryDelay(attempt, o.cfg.retryMin(), o.cfg.retryMax())
	o.log.Warnf("retry %s in %s", leaf.ID, delay)
	o.events.Record("retry", leaf.ID, map[string]any{
		"attempt":  attempt,
		"delay_ms": delay.Milliseconds(),
		"reason":   clip(errMsg, evReasonChars),
	})
	if err := waitBackoff(ctx, delay); err != nil {
		o.parkInterrupted(leaf.ID)
		return err
	}
	return nil
}

// deadlineExceeded reports whether a leaf has spent longer than MaxElapsed
// across all of its attempts. The per-leaf start time is stashed on the
// orchestrator while executeLeaf runs, so the budget covers retries of the
// same leaf rather than restarting each attempt.
func (o *Orchestrator) deadlineExceeded(leafID string, attempt int) bool {
	if o.cfg.MaxElapsed <= 0 {
		return false
	}
	o.leafMu.Lock()
	start, ok := o.leafStart[leafID]
	o.leafMu.Unlock()
	if !ok {
		return false
	}
	if time.Since(start) < o.cfg.MaxElapsed {
		return false
	}
	o.log.Errorf("leaf %s exceeded its time budget (%s over %d attempt(s))",
		leafID, o.cfg.MaxElapsed, attempt)
	o.events.Record("leaf_timeout", leafID, map[string]any{
		"elapsed_ms": o.cfg.MaxElapsed.Milliseconds(),
		"attempts":   attempt,
	})
	return true
}

// giveUpOnDeadline fails a leaf that exhausted its wall-clock budget, reusing
// the normal terminal-failure path so the root surfaces the reason.
func (o *Orchestrator) giveUpOnDeadline(leaf *models.TaskNode, errMsg string) error {
	msg := fmt.Sprintf("gave up after %s (time budget exhausted; raise -max-elapsed to allow more attempts): %s",
		o.cfg.MaxElapsed, truncate(errMsg, 300))
	o.events.Record("leaf_giveup", leaf.ID, map[string]any{
		"reason": "time budget exhausted",
	})
	// Skip the re-split path: a leaf that ran out of time has not proven it
	// is too big, so re-decomposing would restart the same clock.
	if err := o.sched.UpdateNodeState(leaf.ID, models.TaskStateFailed, msg); err != nil {
		return err
	}
	if o.sched.HasFailed() {
		return fmt.Errorf("task %s failed: %s", leaf.ID, truncate(msg, 500))
	}
	return nil
}

func (o *Orchestrator) handleLeafFailure(leaf *models.TaskNode, errMsg string) error {
	live, ok := o.tree.CloneNode(leaf.ID)
	if ok {
		leaf = live
	}
	diagnosis := diagnoseDecompositionFailure(leaf, errMsg)
	o.log.Warnf("%s plan diagnosis: %s", leaf.ID, diagnosis.Summary)

	// Plan defects (especially an expected_output the command never prints)
	// can be repaired in place even at MaxDepth: we re-decompose as atomic
	// with a corrected DoD rather than trying to go deeper. Pure "too big"
	// failures still need remaining depth to split.
	budgetLeft := leaf.DecomposeCount < o.cfg.MaxRedecompose
	canSplit := budgetLeft && leaf.Depth < o.cfg.MaxDepth
	canRepair := budgetLeft && diagnosis.IsPlanDefect()
	if !canSplit && !canRepair {
		// Replan budget is spent, but the leaf's verification command is still
		// in its prompt. Keep retrying instead of marking FAILED: a terminal
		// failure blocks every dependent and the run exits with nothing left
		// to schedule.
		o.log.Warnf("replan budget exhausted on %s; retrying the leaf", leaf.ID)
		o.events.Record("retry", leaf.ID, map[string]any{
			"reason":    clip(errMsg, evReasonChars),
			"diagnosis": clip(diagnosis.Summary, evReasonChars),
			"note":      "replan budget exhausted; keep retrying",
		})
		if err := o.tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
			n.Type = models.NodeTypeLeaf
			n.State = models.TaskStatePending
			n.ErrorMsg = errMsg
			n.ErrorHistory = nil
			return nil
		}); err != nil {
			return err
		}
		o.sched.RefreshAncestors(leaf.ID)
		return nil
	}

	kind := "resplit"
	if diagnosis.IsPlanDefect() {
		kind = "plan_repair"
		o.log.Warnf("re-planning failed leaf %s (decomposition defect)", leaf.ID)
	} else {
		o.log.Warnf("re-splitting failed leaf %s", leaf.ID)
	}
	o.events.Record("leaf_resplit", leaf.ID, map[string]any{
		"reason":    clip(errMsg, evReasonChars),
		"kind":      kind,
		"diagnosis": clip(diagnosis.Summary, evReasonChars),
	})
	err := o.tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
		n.Type = models.NodeTypeCompound
		n.State = models.TaskStatePending
		n.DecomposeCount++
		n.RetryCount = 0
		// Fresh stall window after a plan change: the old identical failures
		// were against a DoD that may no longer apply.
		n.ErrorHistory = nil
		n.ErrorMsg = diagnosis.Constraint + "\n\n" + truncate(errMsg, 1500)
		n.ChildrenIDs = n.ChildrenIDs[:0]
		n.Contract.Constraints = append(n.Contract.Constraints, diagnosis.Constraint)
		return nil
	})
	if err != nil {
		return err
	}
	o.sched.RefreshAncestors(leaf.ID)
	return nil
}

func (o *Orchestrator) checkpoint() error {
	o.cpMu.Lock()
	defer o.cpMu.Unlock()
	if err := o.storage.SaveTree(o.tree); err != nil {
		return err
	}
	latest := filepath.Join(o.cfg.DataDir, "LATEST")
	_ = os.WriteFile(latest, []byte(o.tree.ID+"\n"), 0644)
	// A persisted tree and its live snapshot must never disagree: publish
	// synchronously (bypassing the state-change coalescer).
	o.publishTreeSnapshot("checkpoint")
	return nil
}

// recordSessionStart emits the session banner event for every entry mode
// (run/plan/interactive/guided).
func (o *Orchestrator) recordSessionStart(mode string) {
	saved := o.tree.TotalTokenUsage()
	o.events.Record("session_start", "", map[string]any{
		"mode":          mode,
		"goal":          clip(o.cfg.Goal, evReasonChars),
		"model":         o.cfg.Model,
		"workdir":       o.sandbox.Root,
		"parallel":      o.parallel(),
		"isolate":       o.cfg.Isolate,
		"native_tools":  o.cfg.NativeTools,
		"web":           o.cfg.WebEnabled,
		"browser":       o.cfg.BrowserEnabled,
		"resume_calls":  saved.Calls,
		"resume_tokens": saved.TotalTokens,
	})
}

// recordSessionEnd closes the session event with an outcome classification.
func (o *Orchestrator) recordSessionEnd(mode string, start time.Time, err error) {
	outcome := "completed"
	fields := map[string]any{
		"mode":        mode,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		outcome = "interrupted"
		fields["error"] = clip(err.Error(), evReasonChars)
	case errors.Is(err, ErrPaused):
		outcome = "paused"
	default:
		outcome = "failed"
		fields["error"] = clip(err.Error(), evReasonChars)
	}
	u, calls := o.usage.Total()
	fields["outcome"] = outcome
	fields["calls_this_run"] = calls
	fields["tokens_this_run"] = u.TotalTokens
	// Emit the final tree before session_end so a UI always has a terminal
	// snapshot paired with the outcome event.
	o.publishTreeSnapshot("session_" + outcome)
	o.events.Record("session_end", "", fields)
}

// recordNodeError appends one failed attempt to the node's persistent error
// history (alongside the retry counter and latest-error message). The failure
// fingerprint is stored too: it is what lets the stall detector tell a leaf
// that is repeating itself apart from one that is still making progress.
func (o *Orchestrator) recordNodeError(nodeID, msg string) {
	fp := stallFingerprint(o.projectType(), msg)
	_ = o.tree.UpdateNode(nodeID, func(n *models.TaskNode) error {
		n.RetryCount++
		n.ErrorMsg = msg
		n.ErrorHistory = append(n.ErrorHistory, models.ErrorRecord{
			Time:        time.Now(),
			Error:       msg,
			Fingerprint: fp,
		})
		return nil
	})
}

func (o *Orchestrator) printTree() {
	done, total, pct := o.tree.GetLeafProgress()
	allDone, allTotal, allPct := o.tree.GetProgress()
	o.log.Infof("leaves %d/%d (%.0f%%)  nodes %d/%d (%.0f%%)", done, total, pct, allDone, allTotal, allPct)
	o.log.Print(o.tree.RenderVisualTree())
}

// Status prints the current tree without running.
func (o *Orchestrator) Status() {
	o.printTree()
	o.logUsage()
	o.log.Infof("session file: %s", o.storage.GetTreeFilePath(o.tree.ID))
}
