package agent

import (
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

func TestDiagnoseDecompositionFailure_ExpectedOutput(t *testing.T) {
	leaf := &models.TaskNode{
		ID: "leaf",
		DoD: models.DoD{
			Commands:       []string{"go test -run TestX ./pkg"},
			ExpectedOutput: "PASS",
		},
	}
	msg := "$ go test -run TestX ./pkg\nok\tphys2d/pkg\t0.003s\nexpected output \"PASS\" not found\n"
	d := diagnoseDecompositionFailure(leaf, msg)
	if d.Kind != planKindExpectedOutput {
		t.Fatalf("kind=%v want expected_output", d.Kind)
	}
	if !d.IsPlanDefect() {
		t.Fatal("expected_output mismatch must count as a plan defect")
	}
	if !strings.Contains(d.Constraint, planRepairMarker) {
		t.Fatalf("constraint missing plan-repair marker: %s", d.Constraint)
	}
	if !strings.Contains(d.Constraint, "ok") || !strings.Contains(d.Summary, "PASS") {
		t.Fatalf("diagnosis should mention the mismatch: summary=%q constraint=%q", d.Summary, d.Constraint)
	}
}

func TestDiagnoseDecompositionFailure_TooBig(t *testing.T) {
	leaf := &models.TaskNode{ID: "leaf", DoD: models.DoD{Commands: []string{"go test ./..."}}}
	d := diagnoseDecompositionFailure(leaf, "worker out of steps before finish")
	if d.Kind != planKindTooBig {
		t.Fatalf("kind=%v want too_big", d.Kind)
	}
	if d.IsPlanDefect() {
		t.Fatal("generic failure should not be treated as a DoD defect")
	}
	if strings.Contains(d.Constraint, planRepairMarker) {
		t.Fatal("too-big path should not mark plan-repair")
	}
}

func TestHandleLeafFailure_PlanRepairAtMaxDepth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDepth = 2
	cfg.MaxRedecompose = 2
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "repair_depth"
	cfg.Goal = "goal"
	cfg.Stream = false

	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	// Build a depth-2 leaf under a compound child of root.
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.Type = models.NodeTypeCompound
		n.State = models.TaskStateRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parent := &models.TaskNode{
		ID: "mid", Title: "mid", Type: models.NodeTypeCompound,
		State: models.TaskStateRunning, Depth: 1, ParentID: o.tree.RootID,
	}
	leaf := &models.TaskNode{
		ID: "deep", Title: "deep leaf", Type: models.NodeTypeLeaf,
		State: models.TaskStateRunning, Depth: 2, ParentID: "mid",
		DoD: models.DoD{
			Commands:       []string{"go test ./x"},
			ExpectedOutput: "PASS",
		},
	}
	if err := o.tree.AddNode(parent); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.AddNode(leaf); err != nil {
		t.Fatal(err)
	}
	_ = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.ChildrenIDs = []string{"mid"}
		return nil
	})
	_ = o.tree.UpdateNode("mid", func(n *models.TaskNode) error {
		n.ChildrenIDs = []string{"deep"}
		return nil
	})

	errMsg := "ok\tpkg\nexpected output \"PASS\" not found\n"
	if err := o.handleLeafFailure(leaf, errMsg); err != nil {
		t.Fatal(err)
	}
	live, ok := o.tree.CloneNode("deep")
	if !ok {
		t.Fatal("missing deep")
	}
	if live.State != models.TaskStatePending {
		t.Fatalf("state=%s want PENDING (plan repair, not give-up)", live.State)
	}
	if live.Type != models.NodeTypeCompound {
		t.Fatalf("type=%s want COMPOUND for re-decompose", live.Type)
	}
	if live.DecomposeCount != 1 {
		t.Fatalf("DecomposeCount=%d want 1", live.DecomposeCount)
	}
	if !strings.Contains(live.ErrorMsg, planRepairMarker) {
		t.Fatalf("ErrorMsg should carry plan-repair diagnosis: %s", live.ErrorMsg)
	}
}

func TestHandleLeafFailure_TooBigAtMaxDepthGivesUp(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDepth = 2
	cfg.MaxRedecompose = 2
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "giveup_depth"
	cfg.Goal = "goal"
	cfg.Stream = false

	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	_ = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.Type = models.NodeTypeCompound
		n.State = models.TaskStateRunning
		return nil
	})
	leaf := &models.TaskNode{
		ID: "deep", Title: "deep", Type: models.NodeTypeLeaf,
		State: models.TaskStateRunning, Depth: 2, ParentID: o.tree.RootID,
		DoD: models.DoD{Commands: []string{"go test ./..."}},
	}
	if err := o.tree.AddNode(leaf); err != nil {
		t.Fatal(err)
	}
	_ = o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.ChildrenIDs = []string{"deep"}
		return nil
	})

	if err := o.handleLeafFailure(leaf, "worker out of steps before finish"); err != nil {
		t.Fatal(err)
	}
	live, _ := o.tree.CloneNode("deep")
	if live.State != models.TaskStatePending || live.Type != models.NodeTypeLeaf {
		t.Fatalf("state=%s type=%s, want PENDING leaf so the run keeps retrying", live.State, live.Type)
	}
	if len(live.ErrorHistory) != 0 {
		t.Fatalf("stall history should reset, got %d records", len(live.ErrorHistory))
	}
}
