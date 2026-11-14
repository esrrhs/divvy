package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// verifyRootAcceptance runs the root node's goal-level DoD against the real
// workspace after every child completed. This is the end-to-end check: it
// must exercise the finished deliverable (start the service and hit it, run
// the CLI with real arguments), not just compile one package.
//
// On success the root is flagged IntegrationVerified and becomes truly
// complete. On failure the error is routed to the integration leaf, which is
// reset to PENDING so it re-runs and fixes the assembly. When no integration
// leaf exists the failure is fatal — re-running identical commands cannot
// self-heal without a leaf to fix the problem.
func (o *Orchestrator) verifyRootAcceptance(ctx context.Context) error {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return fmt.Errorf("root node missing")
	}
	if len(root.DoD.Commands) == 0 {
		// Nothing to check: accept rather than hang forever.
		return o.markAcceptancePassed()
	}

	o.log.Actionf("goal acceptance: run end-to-end check")
	vr := o.verify(ctx, o.sandbox, root)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if uerr := vr.UnavailableErr(); uerr != nil {
		return o.abortExecutionUnavailable(o.tree.RootID, vr)
	}
	if vr.OK {
		o.log.Okf("goal acceptance passed")
		return o.markAcceptancePassed()
	}

	o.log.Warnf("goal acceptance failed")
	return o.handleAcceptanceFailure(ctx, vr.Output)
}

func (o *Orchestrator) markAcceptancePassed() error {
	return o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.IntegrationVerified = true
		n.ErrorMsg = ""
		return nil
	})
}

// acceptanceRetryMarker prefixes the ErrorMsg of a leaf that was re-queued
// because the goal-level acceptance failed. executeLeaf reads it (and clears
// it) to seed the retry prompt: the leaf must see the command that actually
// failed, not just its own narrower DoD passing again.
const acceptanceRetryMarker = "[goal-acceptance]"

// consumeAcceptanceRetry returns the seeded retry text for a leaf re-queued by
// an acceptance failure, clearing the marker so it is delivered exactly once.
func (o *Orchestrator) consumeAcceptanceRetry(id string) (string, bool) {
	live, ok := o.tree.CloneNode(id)
	if !ok {
		return "", false
	}
	msg := strings.TrimSpace(live.ErrorMsg)
	if !strings.HasPrefix(msg, acceptanceRetryMarker) {
		return "", false
	}
	seed := strings.TrimSpace(strings.TrimPrefix(msg, acceptanceRetryMarker))
	_ = o.tree.UpdateNode(id, func(n *models.TaskNode) error {
		if strings.HasPrefix(strings.TrimSpace(n.ErrorMsg), acceptanceRetryMarker) {
			n.ErrorMsg = ""
		}
		return nil
	})
	return seed, true
}

func (o *Orchestrator) handleAcceptanceFailure(ctx context.Context, output string) error {
	root, _ := o.tree.CloneNode(o.tree.RootID)
	root.RetryCount++
	_ = o.tree.UpdateNode(root.ID, func(n *models.TaskNode) error {
		n.RetryCount = root.RetryCount
		n.ErrorMsg = output
		return nil
	})

	intLeaf := o.findIntegrationLeaf()
	if intLeaf == "" {
		msg := "goal-level acceptance failed and no integration leaf exists to fix assembly"
		o.log.Errorf("%s", msg)
		return o.sched.UpdateNodeState(root.ID, models.TaskStateFailed, msg)
	}

	if o.cfg.MaxRetries > 0 && root.RetryCount >= o.cfg.MaxRetries {
		msg := fmt.Sprintf("goal acceptance failed after %d attempt(s): %s",
			root.RetryCount, truncate(output, 1000))
		o.log.Errorf("%s", msg)
		return o.sched.UpdateNodeState(root.ID, models.TaskStateFailed, msg)
	}

	// Route the failure to the integration leaf: reset it to PENDING so the
	// main loop re-executes it with the acceptance error. Bubbling makes the
	// root RUNNING again; acceptance is re-run once the leaf completes.
	//
	// The message is not just bookkeeping — executeLeaf turns it into the retry
	// prompt. It has to say out loud that the leaf's own DoD is the weaker bar,
	// because a model that sees only its own green check mark will do nothing.
	requeueMsg := acceptanceRetryMarker + " the whole deliverable failed its final end-to-end check. " +
		"Your own task DoD passing is NOT enough: run the command below yourself, " +
		"fix what it reports, and re-run it before you finish.\n" + truncate(output, 3000)

	// From here on the leaf is verified BY the acceptance. See
	// widenGateToAcceptance for why the leaf's own DoD is not enough.
	o.widenGateToAcceptance(intLeaf, root)

	if err := o.sched.UpdateNodeState(intLeaf, models.TaskStatePending, requeueMsg); err != nil {
		return err
	}
	o.log.Warnf("re-route acceptance failure to integration leaf %s", intLeaf)

	_ = o.checkpoint()
	delay := RetryDelay(root.RetryCount, o.cfg.retryMin(), o.cfg.retryMax())
	o.log.Warnf("retry goal acceptance in %s", delay)
	if err := waitBackoff(ctx, delay); err != nil {
		return err
	}
	return nil
}

// widenGateToAcceptance raises the integration leaf's verification bar to the
// goal-level acceptance.
//
// The leaf's own DoD comes from the planner and is usually NARROWER than the
// acceptance: "go test ./..." passes while the deliverable is broken. Left
// alone, the leaf gets marked COMPLETED and merged, the acceptance fails in
// exactly the same way, and the run ping-pongs between two states without ever
// converging — "I told you the door is unlocked, I went and wiped the window".
//
// From the moment the acceptance has failed once, the leaf that owns the
// deliverable is verified by that acceptance. The tree is assembled by then
// (the integration leaf runs last), so the command is runnable in its mirror.
//
// The leaf's own commands are kept, not replaced: they may check something the
// acceptance does not. The acceptance goes last, so a cheap failure still
// short-circuits before the expensive end-to-end check.
func (o *Orchestrator) widenGateToAcceptance(leafID string, root *models.TaskNode) {
	if root == nil || len(root.DoD.Commands) == 0 {
		return
	}
	leaf, ok := o.tree.CloneNode(leafID)
	if !ok {
		return
	}
	cmds := append([]string{}, leaf.DoD.Commands...)
	for _, c := range root.DoD.Commands {
		if !slices.Contains(cmds, c) {
			cmds = append(cmds, c)
		}
	}
	timeout := leaf.DoD.TimeoutSec
	if root.DoD.TimeoutSec > timeout {
		timeout = root.DoD.TimeoutSec
	}
	_ = o.tree.UpdateNode(leafID, func(n *models.TaskNode) error {
		n.DoD.Commands = cmds
		if n.DoD.TimeoutSec < timeout {
			n.DoD.TimeoutSec = timeout
		}
		return nil
	})
	o.log.Warnf("integration leaf %s is now verified by the goal acceptance: %v", leafID, cmds)
}

// findIntegrationLeaf locates the node responsible for wiring the deliverable
// together. Direct children of the root win, then any leaf in the tree: a
// planner that nests the assembly work under a compound ("delivery") must not
// cost the run its only recovery path.
func (o *Orchestrator) findIntegrationLeaf() string {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return ""
	}

	// Pass 1: direct children — the shape the prompt asks for, where a name
	// like "integrate" is trustworthy.
	if id := o.matchIntegrationLeaf(root.ChildrenIDs, true); id != "" {
		return id
	}
	// Pass 2: anywhere in the tree, but only an actual entry file counts.
	var all []string
	for _, leaf := range o.tree.Leaves() {
		all = append(all, leaf.ID)
	}
	return o.matchIntegrationLeaf(all, false)
}

// matchIntegrationLeaf scans ids for an entry-producing leaf, then (only for
// direct children) for one whose id/title says integrate/entry.
//
// The name fallback is restricted on purpose. A leaf called
// "integrate_collision_resolution" integrates one package's modules; treating
// it as the assembly leaf is how a detected plan gap gets silently swallowed.
func (o *Orchestrator) matchIntegrationLeaf(ids []string, directOnly bool) string {
	var fallback string
	for _, cid := range ids {
		child, ok := o.tree.CloneNode(cid)
		if !ok || child.Type != models.NodeTypeLeaf {
			continue
		}
		if directOnly && child.ParentID != o.tree.RootID {
			continue
		}
		for _, out := range child.Contract.Outputs {
			p := strings.ToLower(strings.TrimSpace(out))
			if p == "main.go" || strings.HasSuffix(p, "/main.go") {
				return cid
			}
		}
		if !directOnly {
			continue
		}
		idTitle := strings.ToLower(child.ID + " " + child.Title)
		if strings.Contains(idTitle, "integrate") || strings.Contains(idTitle, "entry") {
			if fallback == "" {
				fallback = cid
			}
		}
	}
	return fallback
}

// hasIntegrationLeaf reports whether an assembly leaf exists at all.
func (o *Orchestrator) hasIntegrationLeaf() bool {
	return o.findIntegrationLeaf() != ""
}

// ensureIntegrationLeaf appends the assembly leaf a plan failed to produce.
// This is the fallback that keeps a runnable goal recoverable: without a leaf
// to route a goal-level acceptance failure to, the root is marked FAILED and
// no retry or -resume can ever repair it.
//
// The injected leaf depends on every existing sibling, so it runs last, and
// it declares the entry path the goal-level acceptance actually executes —
// redeclaring the DoD as its own contract is what makes the plan consistent.
func (o *Orchestrator) ensureIntegrationLeaf(reason string) (string, error) {
	// Skip only when the gap is genuinely closed. A leaf that merely looks
	// like an assembler is not enough — a run that had already detected the
	// gap still failed because an "integrate_*" leaf was mistaken for one.
	if len(o.planBlockers()) == 0 {
		if id := o.findIntegrationLeaf(); id != "" {
			return id, nil
		}
	}
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return "", fmt.Errorf("root node missing")
	}

	outputs := o.integrationOutputs(root)
	if len(outputs) == 0 {
		return "", fmt.Errorf("cannot infer an entry point for this toolchain")
	}
	deps := append([]string{}, root.ChildrenIDs...)

	id := uniqueID("integrate_entry", o.tree.HasNode)
	title := "Wire the deliverable together and create the entry point"
	desc := "All other subtasks are complete. Assemble them into a runnable deliverable: " +
		"create the entry point, fix import paths and package wiring, and make the " +
		"goal-level acceptance commands pass end to end. Do not rewrite finished work."
	if reason != "" {
		desc += "\n\nPlan gap detected: " + reason
	}

	node := models.NewTaskNode(id, root.ID, title, desc, models.NodeTypeLeaf, root.Depth+1)
	node.Contract = models.ContractSpec{
		Inputs:       append([]string{}, outputs...),
		Outputs:      outputs,
		Dependencies: deps,
		Constraints:  []string{"assemble existing work; do not reimplement completed subtasks"},
	}
	dod := root.DoD
	ensureDoD(&dod, outputs, o.projectType())
	node.DoD = dod
	node.MaxRetries = o.cfg.MaxRetries

	if err := o.tree.AddNode(node); err != nil {
		return "", err
	}
	if err := o.sched.ValidateDependencies(); err != nil {
		return "", err
	}
	// The root had bubbled COMPLETED from its old children; a new PENDING
	// child makes it runnable again, and acceptance must be re-verified after
	// the leaf lands.
	if err := o.tree.UpdateNode(root.ID, func(n *models.TaskNode) error {
		n.State = models.TaskStateRunning
		n.IntegrationVerified = false
		return nil
	}); err != nil {
		return "", err
	}
	o.log.Warnf("plan gap: %s — appended integration leaf %s", reason, id)
	o.events.Record("plan_heal", id, map[string]any{"reason": clip(reason, evReasonChars)})
	return id, nil
}

// integrationOutputs picks the entry file the injected leaf must produce, plus
// the data files the acceptance reads that nothing else creates.
//
// The entry file is the deliverable the acceptance runs. The missing inputs
// are the fixtures it feeds into that deliverable: without them the injected
// leaf inherits a DoD it cannot pass either, and the gap would stay open on
// every resume.
func (o *Orchestrator) integrationOutputs(root *models.TaskNode) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		n := engine.NormalizeOutputPath(p)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, p)
	}

	proj := o.projectType()
	for _, p := range goalCommandPaths(root.DoD.Commands) {
		n := engine.NormalizeOutputPath(p)
		if n == "" {
			continue
		}
		// Already names a file ("app.py", "cmd/demo.go")?
		if base := n[strings.LastIndex(n, "/")+1:]; strings.Contains(base, ".") {
			add(n)
			break
		}
		switch proj {
		case tools.ProjectGo:
			add(n + "/main.go")
		case tools.ProjectRust:
			add(n + "/main.rs")
		case tools.ProjectPython:
			add(n + "/main.py")
		case tools.ProjectNode:
			add(n + "/main.js")
		}
	}
	if len(out) == 0 {
		names := o.entryPointNames()
		if len(names) == 0 {
			return nil
		}
		add(names[0])
	}
	for _, p := range o.uncoveredGoalInputs(root) {
		add(p)
	}
	return out
}

// healPlanStructure repairs a saved tree whose plan cannot assemble. It only
// acts on a tree that already has children and is not mid-decomposition: a
// fresh plan is validated by the decomposer, and a pending root has no shape
// to judge yet.
//
// This is what turns a terminal failure back into a resumable one. A session
// whose root was already marked FAILED because acceptance had nowhere to go
// gets an integration leaf appended and the root made RUNNING again, so the
// next resume actually does work instead of printing the tree and exiting 1.
func (o *Orchestrator) healPlanStructure() {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok || len(root.ChildrenIDs) == 0 {
		return
	}
	if root.State == models.TaskStatePending || root.State == models.TaskStateDecomposing {
		return
	}
	blockers := o.planBlockers()
	if len(blockers) == 0 {
		return
	}
	// A failed child is a different problem: assembly cannot fix it, and
	// depending on it would leave the new leaf permanently unready.
	for _, cid := range root.ChildrenIDs {
		if c, ok := o.tree.CloneNode(cid); ok && c.State == models.TaskStateFailed {
			o.log.Warnf("plan gap not healed: child %s failed; its own failure comes first", cid)
			return
		}
	}
	for _, b := range blockers {
		o.log.Warnf("plan gap: %s", b)
	}
	if _, err := o.ensureIntegrationLeaf(strings.Join(blockers, "; ")); err != nil {
		o.log.Errorf("cannot heal plan: %v", err)
	}
}
