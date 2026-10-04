package engine

import (
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
)

// buildConflictTree creates root → {alpha, beta}, both leaves declaring
// shared.txt as an output.
func buildConflictTree(t *testing.T) (*TaskTree, *Scheduler) {
	t.Helper()
	tree := NewTaskTree("s", "goal", "goal")
	for _, id := range []string{"alpha", "beta"} {
		child, err := tree.AddChild(tree.RootID, id, id, id, models.NodeTypeLeaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.UpdateNode(child.ID, func(n *models.TaskNode) error {
			n.Contract.Outputs = []string{"shared.txt"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return tree, NewScheduler(tree)
}

func idsOf(nodes []*models.TaskNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestGetReadyLeafNodesAvoidingBlocksConflictingOutputs is the regression test
// for scheduling leaves that write the same file concurrently: whichever
// finished last used to silently discard the other's verified work.
func TestGetReadyLeafNodesAvoidingBlocksConflictingOutputs(t *testing.T) {
	_, sched := buildConflictTree(t)

	ready := idsOf(sched.GetReadyLeafNodesAvoiding(nil))
	if len(ready) != 2 {
		t.Fatalf("without claims both leaves should be ready, got %v", ready)
	}

	// Once one leaf holds the claim, its sibling must not be scheduled.
	claimed := OutputSetClaims([]string{"shared.txt"})
	ready = idsOf(sched.GetReadyLeafNodesAvoiding(claimed))
	if len(ready) != 0 {
		t.Fatalf("conflicting leaf should be held back, got %v", ready)
	}
}

// TestGetReadyLeafNodesAvoidingReleasesAfterClaim confirms a non-conflicting
// leaf still runs in parallel while a sibling holds a claim.
func TestGetReadyLeafNodesAvoidingReleasesAfterClaim(t *testing.T) {
	tree := NewTaskTree("s", "goal", "goal")
	for _, spec := range []struct{ id, out string }{
		{"alpha", "shared.txt"},
		{"beta", "other.txt"},
	} {
		child, err := tree.AddChild(tree.RootID, spec.id, spec.id, spec.id, models.NodeTypeLeaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.UpdateNode(child.ID, func(n *models.TaskNode) error {
			n.Contract.Outputs = []string{spec.out}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	sched := NewScheduler(tree)

	ready := idsOf(sched.GetReadyLeafNodesAvoiding(OutputSetClaims([]string{"shared.txt"})))
	if len(ready) != 1 || !hasID(ready, "beta") {
		t.Fatalf("only beta should remain ready, got %v", ready)
	}
}

// TestGetReadyLeafNodesAvoidingEmptyOutputsAlwaysEligible covers a leaf whose
// contract names no file: it cannot be checked, so it is never held back.
func TestGetReadyLeafNodesAvoidingEmptyOutputsAlwaysEligible(t *testing.T) {
	tree := NewTaskTree("s", "goal", "goal")
	child, err := tree.AddChild(tree.RootID, "loose", "loose", "loose", models.NodeTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode(child.ID, func(n *models.TaskNode) error {
		n.Contract.Outputs = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(tree)

	ready := idsOf(sched.GetReadyLeafNodesAvoiding(OutputSetClaims([]string{"anything.txt"})))
	if len(ready) != 1 {
		t.Fatalf("leaf with no declared outputs should stay eligible, got %v", ready)
	}
}

func TestNormalizeOutputPath(t *testing.T) {
	cases := map[string]string{
		"pkg/a.go":     "pkg/a.go",
		"./pkg/a.go":   "pkg/a.go",
		"pkg//a.go":    "pkg/a.go",
		"pkg/./a.go":   "pkg/a.go",
		"  pkg/a.go  ": "pkg/a.go",
		`pkg\a.go`:     "pkg/a.go",
		".":            "",
		"":             "",
		"a/../b.go":    "b.go",
	}
	for in, want := range cases {
		if got := NormalizeOutputPath(in); got != want {
			t.Errorf("NormalizeOutputPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOutputSetClaimsNormalizes guards the mutual-exclusion key: differently
// written but equivalent paths must collapse to the same claim.
func TestOutputSetClaimsNormalizes(t *testing.T) {
	claims := OutputSetClaims([]string{"./pkg/a.go", "pkg/b.go", "pkg//a.go", "  "})
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want 2 distinct entries", claims)
	}
	if !claims["pkg/a.go"] || !claims["pkg/b.go"] {
		t.Fatalf("claims = %v, want pkg/a.go and pkg/b.go", claims)
	}
	if OutputSetClaims(nil) != nil {
		t.Fatal("no outputs should yield no claims")
	}
}

func TestOutputSetConflicts(t *testing.T) {
	claimed := OutputSetClaims([]string{"pkg/a.go"})
	if !OutputSetConflicts([]string{"./pkg/a.go"}, claimed) {
		t.Error("equivalent path should conflict")
	}
	if OutputSetConflicts([]string{"pkg/b.go"}, claimed) {
		t.Error("different path should not conflict")
	}
	if OutputSetConflicts(nil, claimed) {
		t.Error("no outputs should never conflict")
	}
	if OutputSetConflicts([]string{"pkg/a.go"}, nil) {
		t.Error("no claims should never conflict")
	}
}

// TestGetReadyLeafNodesAvoidingRespectsDependencies verifies the claim filter
// does not bypass dependency ordering.
func TestGetReadyLeafNodesAvoidingRespectsDependencies(t *testing.T) {
	tree := NewTaskTree("s", "goal", "goal")
	parent, err := tree.AddChild(tree.RootID, "alpha", "alpha", "alpha", models.NodeTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	child, err := tree.AddChild(parent.ID, "nested", "nested", "nested", models.NodeTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode(child.ID, func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"alpha"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(tree)

	ready := idsOf(sched.GetReadyLeafNodesAvoiding(nil))
	if len(ready) != 1 || ready[0] != "alpha" {
		t.Fatalf("only alpha should be ready before its dependent, got %v", ready)
	}
	if err := sched.UpdateNodeState("alpha", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	ready = idsOf(sched.GetReadyLeafNodesAvoiding(nil))
	if len(ready) != 1 || ready[0] != "nested" {
		t.Fatalf("nested should become ready after alpha, got %v", ready)
	}
}
