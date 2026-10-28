package agent

import (
	"context"
	"fmt"
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
	if err := o.sched.UpdateNodeState(intLeaf, models.TaskStatePending,
		"goal acceptance failed; fix assembly. Error: "+truncate(output, 3000)); err != nil {
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

// integrationOutputs picks the entry file the injected leaf must produce.
// Preference order: the exact path the goal-level acceptance runs, then the
// toolchain's conventional entry name.
func (o *Orchestrator) integrationOutputs(root *models.TaskNode) []string {
	proj := o.projectType()
	for _, p := range goalCommandPaths(root.DoD.Commands) {
		n := engine.NormalizeOutputPath(p)
		if n == "" {
			continue
		}
		// Already names a file ("app.py", "cmd/demo.go")?
		if base := n[strings.LastIndex(n, "/")+1:]; strings.Contains(base, ".") {
			return []string{n}
		}
		switch proj {
		case tools.ProjectGo:
			return []string{n + "/main.go"}
		case tools.ProjectRust:
			return []string{n + "/main.rs"}
		case tools.ProjectPython:
			return []string{n + "/main.py"}
		case tools.ProjectNode:
			return []string{n + "/main.js"}
		}
	}
	names := o.entryPointNames()
	if len(names) == 0 {
		return nil
	}
	return []string{names[0]}
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
