package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
)

// The integration leaf's own DoD comes from the planner and is usually
// NARROWER than the goal-level acceptance. The gap is not cosmetic: it is
// what let a real run mark a leaf complete, merge it, fail the acceptance in
// exactly the same way, and start over — "I told you the door is unlocked, I
// went and wiped the window".
func TestAcceptanceFailure_WidensIntegrationGateToTheAcceptance(t *testing.T) {
	acceptance := "test -f definitely_missing_smoke"
	o := buildAcceptanceTree(t, models.DoD{Commands: []string{acceptance}, TimeoutSec: 10})
	// The leaf's own gate is trivial: it passes no matter what.
	for _, id := range []string{"integrate", "impl"} {
		if err := o.tree.UpdateNode(id, func(n *models.TaskNode) error {
			n.DoD = models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := o.verifyRootAcceptance(context.Background()); err != nil {
		t.Fatal(err)
	}

	integrate, _ := o.tree.CloneNode("integrate")
	if !slices.Contains(integrate.DoD.Commands, acceptance) {
		t.Fatalf("integration leaf is not verified by the acceptance: %v", integrate.DoD.Commands)
	}
	if !slices.Contains(integrate.DoD.Commands, "true") {
		t.Fatalf("the leaf's own check must be kept, not replaced: %v", integrate.DoD.Commands)
	}
	if integrate.DoD.TimeoutSec < 10 {
		t.Fatalf("the acceptance timeout must not shrink the gate: %d", integrate.DoD.TimeoutSec)
	}
	// Only the leaf that owns the deliverable inherits the bar.
	other, _ := o.tree.CloneNode("impl")
	if slices.Contains(other.DoD.Commands, acceptance) {
		t.Fatalf("an unrelated leaf must not inherit the acceptance: %v", other.DoD.Commands)
	}
}

// The widened gate has to actually RUN the acceptance, otherwise the leaf can
// still be marked complete with a broken deliverable.
func TestWidenedGate_FailsWhenTheAcceptanceFails(t *testing.T) {
	acceptance := "test -f definitely_missing_smoke"
	o := buildAcceptanceTree(t, models.DoD{Commands: []string{acceptance}, TimeoutSec: 10})
	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.verifyRootAcceptance(context.Background()); err != nil {
		t.Fatal(err)
	}
	integrate, _ := o.tree.CloneNode("integrate")

	vr := o.verify(context.Background(), o.sandbox, integrate)
	if vr.OK {
		t.Fatal("the leaf must not verify while the acceptance fails")
	}
	if !strings.Contains(vr.Output, "definitely_missing_smoke") {
		t.Fatalf("the failure must name the acceptance command: %q", vr.Output)
	}
}

// The acceptance can fail more than once (the leaf fixes half of it). Widening
// twice must not pile up duplicate commands — every duplicate costs a real
// command run per verification.
func TestWidenedGate_IsIdempotent(t *testing.T) {
	acceptance := "test -f definitely_missing_smoke"
	o := buildAcceptanceTree(t, models.DoD{Commands: []string{acceptance}, TimeoutSec: 10})
	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := o.verifyRootAcceptance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	integrate, _ := o.tree.CloneNode("integrate")
	count := 0
	for _, c := range integrate.DoD.Commands {
		if c == acceptance {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the acceptance appears %d times in the gate: %v", count, integrate.DoD.Commands)
	}
}
