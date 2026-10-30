package agent

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// The wave loop turns one up-front decomposition into a cycle:
//
//	plan a wave → run it → aggregate the whole workspace → plan again
//
// The old shape was open loop: the decomposer had to think the entire goal
// through before a single leaf had produced any code, then every leaf ran to
// its own local DoD, and the project was assembled exactly once at the end.
// A measured 66-leaf run cost 17.8M tokens and still could not run its own demo:
// every leaf was COMPLETED, but no leaf's contract could express "the project
// still builds", and by the time the failure surfaced, the 66 contexts that
// might have explained it were gone.
//
// Closing the loop makes each wave answer that question while the code that
// would break it is still the most recent thing in the workspace. The bet on
// "will this assemble?" stops being one big bet at the end and becomes a small
// bet every few leaves — and a lost small bet is just the next wave's first task.

const (
	// waveAggregateNodeID labels the synthetic node whose DoD the aggregate
	// check runs. It never enters the tree; verify() only needs an ID for its
	// log lines and events.
	waveAggregateNodeID = "wave_aggregate"
	// waveAggregateTimeout is generous on purpose: the aggregate runs the whole
	// project's build and tests, not one package's.
	waveAggregateTimeout = 300
	// waveRepairPrefix names the leaves that fix a broken aggregate.
	waveRepairPrefix = "repair_aggregate"
)

// waveState is the per-run bookkeeping for the plan → execute → aggregate loop.
type waveState struct {
	aggregates int
	fails      int    // consecutive aggregate failures
	repairs    int    // repair leaves injected
	baselineFP string // fingerprint of a workspace that was already broken at start
	landed     atomic.Int32
	// dispatched counts the leaves this wave has launched. The wave bounds
	// execution, not just planning: a planner that emits five leaves in one
	// shot would otherwise run all five before the first aggregate, which is
	// the old "assemble once at the end" behaviour wearing a new name.
	dispatched atomic.Int32
}

// waveEnabled reports whether the run uses the loop at all. -wave 0 restores the
// old flat behaviour exactly: no throttle, no batching, no aggregate, no repair
// leaves.
func (o *Orchestrator) waveEnabled() bool {
	return o.cfg.Wave > 0
}

// noteLeaf records that a leaf landed since the last aggregate.
func (w *waveState) noteLeaf() { w.landed.Add(1) }

// waveBudget is how many more leaves this wave may launch.
func (o *Orchestrator) waveBudget() int {
	if !o.waveEnabled() {
		return 1 << 30
	}
	left := o.cfg.Wave - int(o.wave.dispatched.Load())
	if left < 0 {
		return 0
	}
	return left
}

// waveBoundary reports that a wave is over: nothing is in flight (the caller
// checks), nothing is left to plan, or this wave has launched its quota.
//
// Ending a wave with nothing landed is not a boundary worth aggregating — but
// it does reset the quota, otherwise a wave whose leaves all failed would
// deadlock the scheduler with no room left to launch anything.
func (o *Orchestrator) waveBoundary() bool {
	if !o.waveEnabled() {
		return false
	}
	if o.sched.IsComplete() || o.sched.HasFailed() {
		return false
	}
	full := o.wave.dispatched.Load() >= int32(o.cfg.Wave)
	if !full && o.tree.PendingLeafCount() > 0 {
		return false
	}
	return o.wave.landed.Load() > 0 || full
}

// waveHasRoom reports whether the tree may be expanded further.
//
// The frontier is capped by the number of leaves already planned but not yet
// executed, which is what stops the decomposer from planning fifty leaves
// before the first one has produced any code. readyEmpty forces room: when
// nothing can run, expanding is the only way forward and refusing would
// deadlock the run.
func (o *Orchestrator) waveHasRoom(readyEmpty bool) bool {
	if !o.waveEnabled() {
		return true
	}
	// One expansion at a time. A decomposition in progress has not added its
	// children yet, so PendingLeafCount still reads the old frontier; allowing a
	// second expansion here would let the planner run ahead of the cap by one
	// whole subtree. Waiting costs one round trip.
	if o.planning.Load() > 0 {
		return false
	}
	if readyEmpty {
		return true
	}
	return o.tree.PendingLeafCount() < o.cfg.Wave
}

// aggregateCommands returns the whole-workspace check for this run.
func (o *Orchestrator) aggregateCommands() []string {
	if len(o.cfg.AggregateCommands) > 0 {
		return append([]string(nil), o.cfg.AggregateCommands...)
	}
	return aggregateCommandsFor(o.projectType())
}

// aggregateReady reports whether an aggregate can prove anything yet.
//
// The operator's -aggregate is a contract and always runs. A derived command
// only makes sense once the project actually has code: in a greenfield Go
// module "go build ./..." exits 0 while "go test ./..." exits 1 with "no
// packages to test", so aggregating before the first package exists would
// declare the project broken and spend a whole wave repairing code nobody has
// written.
func (o *Orchestrator) aggregateReady() bool {
	if len(o.cfg.AggregateCommands) > 0 {
		return true
	}
	return o.hasSources(o.projectType())
}

// aggregateCommandsFor picks the cheapest command pair that proves the project
// still stands: build catches broken wiring, tests catch broken behaviour.
//
// It deliberately does not reuse defaultDoDCommands: that one is scoped to a
// leaf's own outputs ("test -f x.go"), and a whole-workspace check must not
// depend on which files a particular leaf declared.
func aggregateCommandsFor(proj tools.ProjectType) []string {
	switch proj {
	case tools.ProjectGo:
		return []string{"go build ./...", "go test ./..."}
	case tools.ProjectNode:
		return []string{"npm test --if-present"}
	case tools.ProjectRust:
		return []string{"cargo build", "cargo test"}
	case tools.ProjectPython:
		return []string{pythonCheckCmd}
	case tools.ProjectCMake:
		return []string{
			"cmake -S . -B build && cmake --build build -j2",
			"ctest --test-dir build --output-on-failure",
		}
	}
	return nil
}

// waveSourceExts are the extensions that make an aggregate meaningful.
var waveSourceExts = map[tools.ProjectType][]string{
	tools.ProjectGo:     {".go"},
	tools.ProjectNode:   {".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx"},
	tools.ProjectRust:   {".rs"},
	tools.ProjectPython: {".py"},
	tools.ProjectCMake:  {".c", ".cc", ".cpp", ".cxx", ".h", ".hpp"},
}

// waveSkipDirs keeps the source scan off dependency trees. Missing one here is
// slow, not wrong.
var waveSkipDirs = map[string]bool{
	".git": true, ".divvy": true, "node_modules": true, "vendor": true,
	"target": true, "__pycache__": true, "dist": true, "build": true,
}

// hasSources reports whether the workspace holds any file in the project's
// language.
//
// Without this the aggregate is not just useless but wrong: in a module that
// has a go.mod and no packages yet, "go build ./..." exits 0 while
// "go test ./..." exits 1 ("no packages to test"). The first wave would then
// be blamed for breaking a project that does not exist yet.
func (o *Orchestrator) hasSources(proj tools.ProjectType) bool {
	exts := waveSourceExts[proj]
	if len(exts) == 0 {
		return false
	}
	found := false
	_ = filepath.WalkDir(o.sandbox.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			if waveSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		for _, e := range exts {
			if strings.HasSuffix(d.Name(), e) {
				found = true
				return nil
			}
		}
		return nil
	})
	return found
}

// baselineAggregate records where the workspace stood before any leaf ran, so a
// project that was already broken is not blamed on the first wave. It is a
// no-op when there is nothing to check yet.
func (o *Orchestrator) baselineAggregate(ctx context.Context) {
	cmds := o.aggregateCommands()
	if len(cmds) == 0 {
		o.log.Debugf("wave aggregate: nothing to check yet")
		return
	}
	// A baseline is only meaningful for a project that already exists. In a
	// greenfield workspace the aggregate fails by definition — there is nothing
	// to build yet — and treating that as pre-existing breakage would excuse
	// every later failure, which is precisely the failure mode this loop exists
	// to catch. So this check is stricter than aggregateReady: even an explicit
	// -aggregate gets no baseline until the workspace has code of its own.
	if !o.hasSources(o.projectType()) {
		o.log.Debugf("wave aggregate baseline: no sources yet, so the run owns the result")
		return
	}
	node := &models.TaskNode{ID: waveAggregateNodeID, DoD: models.DoD{
		Commands: cmds, TimeoutSec: waveAggregateTimeout,
	}}
	vr := o.verify(ctx, o.sandbox, node)
	if ctx.Err() != nil {
		return
	}
	if vr.OK {
		o.log.Infof("wave aggregate baseline: the untouched workspace already builds and tests clean")
		return
	}
	// Remember how it was broken. A wave that reproduces the same failure did
	// not cause it, and queueing a repair leaf for pre-existing breakage would
	// spend the run fixing someone else's tree.
	o.wave.baselineFP = stallFingerprint(o.projectType(), vr.Command+"\n"+vr.Output)
	o.log.Warnf("wave aggregate baseline: the workspace is ALREADY broken before any leaf runs (%s); "+
		"waves that reproduce this failure will not be repaired", vr.Command)
	o.events.Record("wave_baseline", "", map[string]any{
		"ok": false, "command": vr.Command, "output": clip(vr.Output, evReasonChars),
	})
}

// runWaveAggregate checks that the workspace still builds and tests after the
// wave that just landed, and turns a failure into the next wave's first task.
func (o *Orchestrator) runWaveAggregate(ctx context.Context) error {
	landed := int(o.wave.landed.Swap(0))
	// Reset the launch quota even when nothing landed: the wave is over either
	// way, and leaving it spent would stop the next wave from starting.
	o.wave.dispatched.Store(0)
	if landed == 0 {
		return nil
	}
	cmds := o.aggregateCommands()
	if len(cmds) == 0 {
		o.log.Debugf("wave aggregate skipped: no check available for this toolchain yet")
		return nil
	}
	if !o.aggregateReady() {
		o.log.Debugf("wave aggregate skipped: no %s sources in the workspace yet", o.projectType())
		return nil
	}
	o.wave.aggregates++
	o.log.Actionf("wave %d: aggregate check over the whole workspace (%d leaf(s) landed)",
		o.wave.aggregates, landed)

	node := &models.TaskNode{ID: waveAggregateNodeID, DoD: models.DoD{
		Commands: cmds, TimeoutSec: waveAggregateTimeout,
	}}
	vr := o.verify(ctx, o.sandbox, node)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if vr.OK {
		o.wave.fails = 0
		o.events.Record("wave_aggregate", "", map[string]any{
			"cycle": o.wave.aggregates, "ok": true, "leaves": landed,
		})
		o.log.Okf("wave %d aggregate passed", o.wave.aggregates)
		return nil
	}

	o.wave.fails++
	o.events.Record("wave_aggregate", "", map[string]any{
		"cycle": o.wave.aggregates, "ok": false, "leaves": landed,
		"command": vr.Command, "output": clip(vr.Output, evReasonChars),
	})
	o.log.Warnf("wave %d aggregate FAILED: %s", o.wave.aggregates, vr.Command)

	if fp := stallFingerprint(o.projectType(), vr.Command+"\n"+vr.Output); fp != "" && fp == o.wave.baselineFP {
		o.log.Warnf("wave %d aggregate fails exactly like the untouched workspace did; not queuing a repair", o.wave.aggregates)
		return nil
	}

	if o.cfg.MaxAggregateFails > 0 && o.wave.fails >= o.cfg.MaxAggregateFails {
		msg := fmt.Sprintf("the workspace never built cleanly across %d consecutive waves; last failure (%s):\n%s",
			o.wave.fails, vr.Command, truncate(vr.Output, 1000))
		o.log.Errorf("%s", msg)
		if err := o.sched.UpdateNodeState(o.tree.RootID, models.TaskStateFailed, msg); err != nil {
			return err
		}
		_ = o.checkpoint()
		return fmt.Errorf("%s", msg)
	}

	id, err := o.ensureRepairLeaf(cmds, vr)
	if err != nil {
		return err
	}
	_ = o.checkpoint()
	delay := RetryDelay(o.wave.fails-1, o.cfg.retryMin(), o.cfg.retryMax())
	o.log.Warnf("wave %d: queued repair leaf %s; the next wave starts with it (backoff %s)",
		o.wave.aggregates, id, delay)
	if err := waitBackoff(ctx, delay); err != nil {
		return err
	}
	return nil
}

// ensureRepairLeaf appends the task that fixes a broken aggregate.
//
// It declares no outputs and no dependencies: it edits files that already
// exist, and it must be runnable immediately — waiting on anything would let
// more work pile up on top of a workspace that is already broken.
func (o *Orchestrator) ensureRepairLeaf(cmds []string, vr VerifyResult) (string, error) {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return "", fmt.Errorf("root node missing")
	}
	id := uniqueID(waveRepairPrefix, o.tree.HasNode)
	desc := fmt.Sprintf(`The workspace no longer builds or passes its tests. The most recent wave of subtasks left it broken.

Failing command: %s

%s

Repair the regression: find what the last wave broke, fix it, and leave working code alone. Reproduce it with the failing command, fix the cause, and run the command again until it passes. Do not rewrite unrelated parts of the project, and do not weaken or delete a test to make it pass.`,
		vr.Command, truncate(vr.Output, 4000))

	node := models.NewTaskNode(id, root.ID,
		"Repair the workspace so it builds and tests again", desc,
		models.NodeTypeLeaf, root.Depth+1)
	node.Contract = models.ContractSpec{
		Inputs:      []string{"the failing command and its output, quoted in the description"},
		Constraints: []string{"fix the regression only; do not rewrite unrelated files", "the whole workspace must build and pass its tests"},
	}
	node.DoD = models.DoD{Commands: append([]string(nil), cmds...), TimeoutSec: waveAggregateTimeout}
	node.MaxRetries = o.cfg.MaxRetries

	if err := o.tree.AddNode(node); err != nil {
		return "", err
	}
	// The root had bubbled COMPLETED (or would have); a new PENDING child makes
	// it runnable again, and acceptance must be re-verified after the leaf lands.
	if err := o.tree.UpdateNode(root.ID, func(n *models.TaskNode) error {
		n.State = models.TaskStateRunning
		n.IntegrationVerified = false
		return nil
	}); err != nil {
		return "", err
	}
	o.wave.repairs++
	o.log.Warnf("aggregate broken after wave %d: queued repair leaf %s", o.wave.aggregates, id)
	o.events.Record("wave_repair", id, map[string]any{
		"cycle":   o.wave.aggregates,
		"command": vr.Command,
		"output":  clip(vr.Output, evReasonChars),
	})
	return id, nil
}
