package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/esrrhs/divvy/pkg/models"
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

// findIntegrationLeaf locates the direct child of the root responsible for
// wiring the deliverable together. Prefer a leaf whose outputs name an entry
// file (main.go), otherwise one whose id/title says integrate/entry/main.
func (o *Orchestrator) findIntegrationLeaf() string {
	var fallback string
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return ""
	}
	for _, cid := range root.ChildrenIDs {
		child, ok := o.tree.CloneNode(cid)
		if !ok || child.Type != models.NodeTypeLeaf {
			continue
		}
		for _, out := range child.Contract.Outputs {
			p := strings.ToLower(strings.TrimSpace(out))
			if p == "main.go" || strings.HasSuffix(p, "/main.go") {
				return cid
			}
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
