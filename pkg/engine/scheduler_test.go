package engine

import (
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/models"
)

func TestScheduler_DependencyResolutionAndReadyNodes(t *testing.T) {
	tree := NewTaskTree("sess_sched", "Root Task", "Root Desc")
	scheduler := NewScheduler(tree)

	// Initially, root is a compound node with no children and Pending -> should be ready to decompose
	decomposable := scheduler.GetNextDecomposableNode()
	if decomposable == nil || decomposable.ID != "root" {
		t.Fatalf("expected root to be decomposable, got %+v", decomposable)
	}

	// Add two children: leaf_A and leaf_B, where leaf_B depends on leaf_A
	_, err := tree.AddChild("root", "leaf_A", "Leaf A", "First step", models.NodeTypeLeaf)
	if err != nil {
		t.Fatalf("failed to add leafA: %v", err)
	}
	leafB, err := tree.AddChild("root", "leaf_B", "Leaf B", "Second step", models.NodeTypeLeaf)
	if err != nil {
		t.Fatalf("failed to add leafB: %v", err)
	}
	leafB.Contract.Dependencies = append(leafB.Contract.Dependencies, "leaf_A")

	// Validate dependencies: should pass
	if err := scheduler.ValidateDependencies(); err != nil {
		t.Fatalf("unexpected dependency error: %v", err)
	}

	// Ready leaf nodes should only be leaf_A (because leaf_B depends on leaf_A)
	readyLeaves := scheduler.GetReadyLeafNodes()
	if len(readyLeaves) != 1 || readyLeaves[0].ID != "leaf_A" {
		t.Fatalf("expected readyLeaves to contain only leaf_A, got %d items", len(readyLeaves))
	}

	// Complete leaf_A
	if err := scheduler.UpdateNodeState("leaf_A", models.TaskStateCompleted, ""); err != nil {
		t.Fatalf("failed to update leaf_A state: %v", err)
	}

	// Now leaf_B should be ready
	readyLeaves = scheduler.GetReadyLeafNodes()
	if len(readyLeaves) != 1 || readyLeaves[0].ID != "leaf_B" {
		t.Fatalf("expected readyLeaves to contain leaf_B, got %d items", len(readyLeaves))
	}

	// Complete leaf_B
	if err := scheduler.UpdateNodeState("leaf_B", models.TaskStateCompleted, ""); err != nil {
		t.Fatalf("failed to update leaf_B state: %v", err)
	}

	// Now root should have automatically bubbled up to COMPLETED!
	root := tree.GetRoot()
	if root.State != models.TaskStateCompleted {
		t.Fatalf("expected root to auto-complete, got %s", root.State)
	}

	// Compound root must pass goal-level acceptance before truly complete.
	if !scheduler.RootNeedsAcceptance() || scheduler.IsComplete() {
		t.Fatal("root should need acceptance at this point")
	}
	if err := tree.UpdateNode("root", func(n *models.TaskNode) error {
		n.IntegrationVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !scheduler.IsComplete() {
		t.Fatalf("expected scheduler.IsComplete() to be true")
	}
}

func TestScheduler_CircularDependencyDetection(t *testing.T) {
	tree := NewTaskTree("sess_cycle", "Root", "Root")
	node1, _ := tree.AddChild("root", "n1", "Node 1", "Desc", models.NodeTypeLeaf)
	node2, _ := tree.AddChild("root", "n2", "Node 2", "Desc", models.NodeTypeLeaf)

	node1.Contract.Dependencies = []string{"n2"}
	node2.Contract.Dependencies = []string{"n1"}

	scheduler := NewScheduler(tree)
	err := scheduler.ValidateDependencies()
	if err == nil {
		t.Fatalf("expected circular dependency error, got nil")
	}
}

func TestScheduler_ThreeLevelCompletion(t *testing.T) {
	tree := NewTaskTree("sess_3", "Root", "Root")
	if _, err := tree.AddChild("root", "mid", "Mid", "Mid", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.AddChild("mid", "leaf", "Leaf", "Leaf", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(tree)
	if err := s.UpdateNodeState("leaf", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	mid, _ := tree.GetNode("mid")
	if mid.State != models.TaskStateCompleted {
		t.Fatalf("mid=%s", mid.State)
	}
	// Root bubbled COMPLETED but awaits goal-level acceptance.
	if !s.RootNeedsAcceptance() {
		t.Fatal("root should need acceptance")
	}
	if err := tree.UpdateNode("root", func(n *models.TaskNode) error {
		n.IntegrationVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !s.IsComplete() {
		t.Fatalf("root=%s", tree.GetRoot().State)
	}
}
