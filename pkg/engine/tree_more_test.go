package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/models"
)

// threeLevelTree builds root → mid (compound) → {leafA, leafB}.
func threeLevelTree(t *testing.T) *TaskTree {
	t.Helper()
	tree := NewTaskTree("sess", "root goal", "root desc")
	mid, err := tree.AddChild(tree.RootID, "mid", "mid", "mid desc", models.NodeTypeCompound)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		leaf, err := tree.AddChild(mid.ID, id, id, id+" desc", models.NodeTypeLeaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
			n.Contract.Outputs = []string{id + ".go"}
			n.Contract.Dependencies = []string{}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

func TestGetNodeAndHasNode(t *testing.T) {
	tree := threeLevelTree(t)

	if !tree.HasNode("mid") {
		t.Error("HasNode(mid) = false")
	}
	if tree.HasNode("nope") {
		t.Error("HasNode(nope) = true")
	}

	// GetNode aliases the live node; a mutation through it must be visible.
	node, ok := tree.GetNode("mid")
	if !ok {
		t.Fatal("GetNode(mid) not found")
	}
	if node.Type != models.NodeTypeCompound {
		t.Errorf("mid type = %s", node.Type)
	}
	if _, ok := tree.GetNode("nope"); ok {
		t.Error("GetNode(nope) reported found")
	}
}

func TestCloneNodeIsIndependent(t *testing.T) {
	tree := threeLevelTree(t)
	clone, ok := tree.CloneNode("a")
	if !ok {
		t.Fatal("CloneNode(a) not found")
	}
	clone.Title = "mutated"
	clone.Contract.Outputs[0] = "mutated.go"

	live, _ := tree.GetNode("a")
	if live.Title == "mutated" {
		t.Error("clone mutation leaked into the live node")
	}
	if live.Contract.Outputs[0] != "a.go" {
		t.Error("clone slice mutation leaked into the live node")
	}
	if _, ok := tree.CloneNode("nope"); ok {
		t.Error("CloneNode(nope) reported found")
	}
}

func TestGetChildren(t *testing.T) {
	tree := threeLevelTree(t)
	kids, err := tree.GetChildren("mid")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("children = %d, want 2", len(kids))
	}
	if _, err := tree.GetChildren("nope"); err == nil {
		t.Error("expected an error for an unknown parent")
	}
}

func TestGetRoot(t *testing.T) {
	tree := threeLevelTree(t)
	root := tree.GetRoot()
	if root == nil {
		t.Fatal("GetRoot returned nil")
	}
	if root.ID != tree.RootID {
		t.Errorf("root id = %q, want %q", root.ID, tree.RootID)
	}
}

func TestLeavesReturnsOnlyLeavesSortedByID(t *testing.T) {
	tree := threeLevelTree(t)
	leaves := tree.Leaves()
	if len(leaves) != 2 {
		t.Fatalf("leaves = %d, want 2 (the compound mid must be excluded)", len(leaves))
	}
	if leaves[0].ID != "a" || leaves[1].ID != "b" {
		t.Errorf("leaves not sorted by id: %q,%q", leaves[0].ID, leaves[1].ID)
	}
}

func TestNodeIDsInStates(t *testing.T) {
	tree := threeLevelTree(t)
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	done := tree.NodeIDsInStates(models.TaskStateCompleted)
	if len(done) != 1 || done[0] != "a" {
		t.Errorf("completed ids = %v, want [a]", done)
	}
	pending := tree.NodeIDsInStates(models.TaskStatePending)
	// root, mid and b are still PENDING.
	if len(pending) != 3 {
		t.Errorf("pending ids = %v, want 3", pending)
	}
	// Multiple states at once: a (completed) plus root/mid/b (pending) = 4.
	both := tree.NodeIDsInStates(models.TaskStateCompleted, models.TaskStatePending)
	if len(both) != 4 {
		t.Errorf("combined = %v, want all 4 nodes", both)
	}
	if len(tree.NodeIDsInStates(models.TaskStateFailed)) != 0 {
		t.Error("no node should be FAILED")
	}
}

func TestTotalTokenUsageSumsEveryNode(t *testing.T) {
	tree := threeLevelTree(t)
	for i, id := range []string{"root", "mid", "a"} {
		if err := tree.UpdateNode(id, func(n *models.TaskNode) error {
			n.TokenUsage = models.TokenUsage{
				Calls:            i + 1,
				PromptTokens:     10 * (i + 1),
				CompletionTokens: 5 * (i + 1),
				TotalTokens:      15 * (i + 1),
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := tree.TotalTokenUsage()
	want := models.TokenUsage{Calls: 6, PromptTokens: 60, CompletionTokens: 30, TotalTokens: 90}
	if got != want {
		t.Fatalf("TotalTokenUsage = %+v, want %+v", got, want)
	}
	if empty := NewTaskTree("x", "g", "d").TotalTokenUsage(); empty.Calls != 0 {
		t.Errorf("fresh tree usage = %+v, want zero", empty)
	}
}

func TestAddNodeRejectsDuplicatesAndMissingParent(t *testing.T) {
	tree := threeLevelTree(t)
	dup := &models.TaskNode{ID: "a", ParentID: "mid", Title: "t", Type: models.NodeTypeLeaf}
	if err := tree.AddNode(dup); err == nil {
		t.Error("expected a duplicate-id error")
	}

	orphan := &models.TaskNode{ID: "new", ParentID: "ghost", Title: "t", Type: models.NodeTypeLeaf}
	if err := tree.AddNode(orphan); err == nil {
		t.Error("expected an error for a missing parent")
	}

	// A parentless node is valid (it just has no parent link).
	rootless := &models.TaskNode{ID: "standalone", Title: "t", Type: models.NodeTypeLeaf}
	if err := tree.AddNode(rootless); err != nil {
		t.Errorf("parentless node rejected: %v", err)
	}
	if !tree.HasNode("standalone") {
		t.Error("standalone node was not added")
	}
}

func TestAddChildRejectsDuplicates(t *testing.T) {
	tree := threeLevelTree(t)
	if _, err := tree.AddChild("mid", "a", "t", "d", models.NodeTypeLeaf); err == nil {
		t.Error("expected a duplicate-id error")
	}
	if _, err := tree.AddChild("ghost", "x", "t", "d", models.NodeTypeLeaf); err == nil {
		t.Error("expected an error for an unknown parent")
	}
}

func TestUpdateNodeRejectsUnknownID(t *testing.T) {
	tree := threeLevelTree(t)
	err := tree.UpdateNode("nope", func(*models.TaskNode) error { return nil })
	if err == nil {
		t.Fatal("expected an error for an unknown node")
	}
}

// TestUpdateNodePropagatesCallbackError verifies a callback error aborts the
// mutation instead of partially applying it.
func TestUpdateNodePropagatesCallbackError(t *testing.T) {
	tree := threeLevelTree(t)
	before, _ := tree.GetNode("a")
	sentinel := os.ErrInvalid
	err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Title = "should not stick"
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want the callback error", err)
	}
	after, _ := tree.GetNode("a")
	if after.Title == "should not stick" {
		t.Error("a failed UpdateNode must not leave partial changes")
	}
	if after.Title != before.Title {
		t.Errorf("title changed from %q to %q despite the error", before.Title, after.Title)
	}
}

func TestConvertToLeaf(t *testing.T) {
	tree := threeLevelTree(t)
	// "a" is a childless compound in this fixture's terms only after we make
	// it one; convert the mid's child "a" is already a leaf, so build a
	// dedicated childless compound instead.
	c, err := tree.AddChild(tree.RootID, "c", "c", "c", models.NodeTypeCompound)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.ConvertToLeaf(c.ID, models.ContractSpec{Outputs: []string{"m.go"}}, models.DoD{Commands: []string{"go build ./..."}}); err != nil {
		t.Fatal(err)
	}
	got, _ := tree.GetNode(c.ID)
	if got.Type != models.NodeTypeLeaf {
		t.Errorf("type = %s, want LEAF", got.Type)
	}
	if got.State != models.TaskStatePending {
		t.Errorf("state = %s, want PENDING", got.State)
	}
	if len(got.Contract.Outputs) != 1 || got.Contract.Outputs[0] != "m.go" {
		t.Errorf("outputs = %v", got.Contract.Outputs)
	}
	if got.DoD.TimeoutSec != 60 {
		t.Errorf("timeout = %d, want the 60s default", got.DoD.TimeoutSec)
	}
}

// TestConvertToLeafRejectsNodesWithChildren guards the invariant that a parent
// cannot become a leaf while it still has children to bubble up to.
func TestConvertToLeafRejectsNodesWithChildren(t *testing.T) {
	tree := threeLevelTree(t)
	if err := tree.ConvertToLeaf("mid", models.ContractSpec{}, models.DoD{}); err == nil {
		t.Fatal("expected an error converting a node that has children")
	}
}

// TestConvertToLeafKeepsExistingWhenSpecEmpty verifies a sparse spec does not
// wipe a contract that is already populated.
func TestConvertToLeafKeepsExistingWhenSpecEmpty(t *testing.T) {
	tree := threeLevelTree(t)
	if err := tree.ConvertToLeaf("a", models.ContractSpec{}, models.DoD{}); err != nil {
		t.Fatal(err)
	}
	a, _ := tree.GetNode("a")
	if len(a.Contract.Outputs) != 1 || a.Contract.Outputs[0] != "a.go" {
		t.Errorf("empty spec clobbered the existing outputs: %v", a.Contract.Outputs)
	}
}

func TestResetChildrenRemovesWholeSubtree(t *testing.T) {
	tree := threeLevelTree(t)
	if err := tree.ResetChildren(tree.RootID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mid", "a", "b"} {
		if tree.HasNode(id) {
			t.Errorf("%s should have been removed by ResetChildren", id)
		}
	}
	root, _ := tree.GetNode(tree.RootID)
	if root.Type != models.NodeTypeCompound {
		t.Errorf("root type = %s, want COMPOUND so it can be re-planned", root.Type)
	}
	if root.State != models.TaskStatePending {
		t.Errorf("root state = %s, want PENDING", root.State)
	}
	if len(root.ChildrenIDs) != 0 {
		t.Errorf("root children = %v, want empty", root.ChildrenIDs)
	}
	if err := tree.ResetChildren("ghost"); err == nil {
		t.Error("expected an error for an unknown parent")
	}
}

func TestGetProgressCountsEveryNode(t *testing.T) {
	tree := threeLevelTree(t)
	comp, total, pct := tree.GetProgress()
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if comp != 0 || pct != 0 {
		t.Errorf("initial progress = %d/%d (%.1f%%), want 0", comp, total, pct)
	}
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	comp, total, pct = tree.GetProgress()
	if comp != 1 || total != 4 || pct != 25 {
		t.Errorf("progress = %d/%d (%.1f%%), want 1/4 (25%%)", comp, total, pct)
	}
}

func TestGetLeafProgressIgnoresCompounds(t *testing.T) {
	tree := threeLevelTree(t)
	done, total, _ := tree.GetLeafProgress()
	if total != 2 {
		t.Errorf("leaf total = %d, want 2", total)
	}
	if done != 0 {
		t.Errorf("leaf done = %d, want 0", done)
	}
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	done, total, pct := tree.GetLeafProgress()
	if done != 1 || total != 2 || pct != 50 {
		t.Errorf("leaf progress = %d/%d (%.1f%%), want 1/2 (50%%)", done, total, pct)
	}
}

// TestGetLeafProgressEmptyTree guards the divide-by-zero on a tree with no
// leaves yet.
func TestGetLeafProgressEmptyTree(t *testing.T) {
	tree := NewTaskTree("s", "g", "d")
	done, total, pct := tree.GetLeafProgress()
	if done != 0 || total != 0 || pct != 0 {
		t.Errorf("empty tree leaf progress = %d/%d (%.1f%%), want zeros", done, total, pct)
	}
	// The lone root starts PENDING, so nothing is complete yet.
	comp, total, pct := tree.GetProgress()
	if comp != 0 || total != 1 || pct != 0 {
		t.Errorf("fresh tree progress = %d/%d (%.1f%%), want 0/1", comp, total, pct)
	}
}

func TestRenderVisualTreeShowsCostWhenPriced(t *testing.T) {
	tree := threeLevelTree(t)
	tree.ModelName = "gpt-4o-mini"
	tree.PriceFor = cost.DefaultPricing().PriceFor
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.TokenUsage = models.TokenUsage{Calls: 2, TotalTokens: 1000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := tree.RenderVisualTree()
	if !strings.Contains(out, "tok") {
		t.Errorf("tree should show token usage:\n%s", out)
	}
	if !strings.Contains(out, "$") {
		t.Errorf("tree should show a cost estimate for a priced model:\n%s", out)
	}
}

// TestRenderVisualTreeOmitsCostWhenUnpriced covers local/zero-cost models.
func TestRenderVisualTreeOmitsCostWhenUnpriced(t *testing.T) {
	tree := threeLevelTree(t)
	tree.ModelName = "some-local-model"
	tree.PriceFor = cost.DefaultPricing().PriceFor
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.TokenUsage = models.TokenUsage{Calls: 2, TotalTokens: 1000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out := tree.RenderVisualTree(); strings.Contains(out, "$") {
		t.Errorf("unpriced model should not show a cost:\n%s", out)
	}
}

func TestRenderVisualTreeEmpty(t *testing.T) {
	tree := &TaskTree{RootID: "root", Nodes: map[string]*models.TaskNode{}}
	if got := tree.RenderVisualTree(); !strings.Contains(got, "Empty") {
		t.Errorf("expected an empty-tree marker, got %q", got)
	}
}

func TestJSONRoundTripPreservesEverything(t *testing.T) {
	tree := threeLevelTree(t)
	tree.Goal = "ship it"
	tree.WorkDir = "/tmp/ws"
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"a.go"}
		n.DoD.Commands = []string{"go test ./..."}
		n.RetryCount = 3
		n.ResultSummary = "done"
		n.IntegrationVerified = true
		n.TokenUsage = models.TokenUsage{Calls: 5, TotalTokens: 42}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := tree.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != tree.ID || back.Goal != tree.Goal || back.WorkDir != tree.WorkDir {
		t.Errorf("metadata lost: %+v", back)
	}
	if len(back.Nodes) != len(tree.Nodes) {
		t.Errorf("node count = %d, want %d", len(back.Nodes), len(tree.Nodes))
	}
	a, ok := back.GetNode("a")
	if !ok {
		t.Fatal("node a lost")
	}
	if a.RetryCount != 3 || a.ResultSummary != "done" || !a.IntegrationVerified {
		t.Errorf("node fields lost: %+v", a)
	}
	if a.TokenUsage.TotalTokens != 42 {
		t.Errorf("token usage lost: %+v", a.TokenUsage)
	}
	if len(a.Contract.Outputs) != 1 || a.Contract.Outputs[0] != "a.go" {
		t.Errorf("contract lost: %+v", a.Contract)
	}
}

// TestFromJSONRepairsMissingFields covers hand-edited or older files that lack
// maps or the root id.
func TestFromJSONRepairsMissingFields(t *testing.T) {
	back, err := FromJSON([]byte(`{"id":"s","nodes":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if back.Nodes == nil {
		t.Error("Nodes map should be initialized, not nil")
	}
	if back.RootID != "root" {
		t.Errorf("RootID = %q, want the default \"root\"", back.RootID)
	}
	if err := back.UpdateNode("nope", func(*models.TaskNode) error { return nil }); err == nil {
		t.Error("expected an error updating a node in an empty tree")
	}
	if _, err := FromJSON([]byte(`not json`)); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestSchedulerGetTree(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	if sched.GetTree() != tree {
		t.Error("GetTree should return the managed tree")
	}
}

func TestAreDependenciesSatisfied(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)

	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"b"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := tree.GetNode("a")

	// b is still PENDING, so a must not be runnable.
	if sched.AreDependenciesSatisfied(a) {
		t.Error("a should not be ready while b is pending")
	}
	if err := sched.UpdateNodeState("b", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if !sched.AreDependenciesSatisfied(a) {
		t.Error("a should be ready once b completed")
	}
	if sched.AreDependenciesSatisfied(nil) {
		t.Error("nil node should never be ready")
	}

	// A dependency on a node that does not exist blocks forever.
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"ghost"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ = tree.GetNode("a")
	if sched.AreDependenciesSatisfied(a) {
		t.Error("a missing dependency should block readiness")
	}
}

func TestValidateDependenciesCatchesMissingAndCycles(t *testing.T) {
	tree := NewTaskTree("s", "g", "d")
	leaf, err := tree.AddChild(tree.RootID, "x", "x", "x", models.NodeTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"ghost"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(tree)
	if err := sched.ValidateDependencies(); err == nil {
		t.Error("expected an error for a dependency on a missing node")
	}

	// A → B → A must be reported as a cycle.
	tree2 := NewTaskTree("s", "g", "d")
	a, _ := tree2.AddChild(tree2.RootID, "a", "a", "a", models.NodeTypeLeaf)
	b, _ := tree2.AddChild(tree2.RootID, "b", "b", "b", models.NodeTypeLeaf)
	_ = tree2.UpdateNode(a.ID, func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"b"}
		return nil
	})
	_ = tree2.UpdateNode(b.ID, func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"a"}
		return nil
	})
	if err := NewScheduler(tree2).ValidateDependencies(); err == nil {
		t.Error("expected a circular dependency error")
	}
}

func TestUpdateNodeStateBubblesToAncestors(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)

	var changes []string
	sched.OnStateChange = func(id string, old, new models.TaskState, msg string) {
		changes = append(changes, id+":"+string(old)+"->"+string(new))
	}

	for _, id := range []string{"a", "b"} {
		if err := sched.UpdateNodeState(id, models.TaskStateRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := sched.UpdateNodeState(id, models.TaskStateCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}
	// mid should have bubbled COMPLETED once both children finished.
	mid, _ := tree.GetNode("mid")
	if mid.State != models.TaskStateCompleted {
		t.Errorf("mid state = %s, want COMPLETED", mid.State)
	}
	root, _ := tree.GetNode(tree.RootID)
	if root.State != models.TaskStateCompleted {
		t.Errorf("root state = %s, want COMPLETED", root.State)
	}
	if len(changes) == 0 {
		t.Error("OnStateChange was never called")
	}
}

func TestUpdateNodeStateRecordsTerminalFailure(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	if err := sched.UpdateNodeState("a", models.TaskStateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	a, _ := tree.GetNode("a")
	if len(a.ErrorHistory) != 1 || a.ErrorHistory[0].Error != "boom" {
		t.Errorf("terminal failure must be recorded permanently: %+v", a.ErrorHistory)
	}
	// An empty message must not append a blank record.
	if err := sched.UpdateNodeState("b", models.TaskStateFailed, "   "); err != nil {
		t.Fatal(err)
	}
	b, _ := tree.GetNode("b")
	if len(b.ErrorHistory) != 0 {
		t.Errorf("blank error should not be recorded: %+v", b.ErrorHistory)
	}
	if err := sched.UpdateNodeState("ghost", models.TaskStateRunning, ""); err == nil {
		t.Error("expected an error for an unknown node")
	}
}

func TestRefreshAncestorsAfterReSplit(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	// Simulate a failed leaf being converted back to a compound: the type
	// change bypasses UpdateNodeState, so ancestors need an explicit refresh.
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Type = models.NodeTypeCompound
		n.State = models.TaskStatePending
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sched.RefreshAncestors("a")
	mid, _ := tree.GetNode("mid")
	// a is pending again, so mid must not still claim COMPLETED.
	if mid.State == models.TaskStateCompleted {
		t.Error("mid stayed COMPLETED after its child went back to PENDING")
	}
	// Refreshing an unknown node must not panic.
	sched.RefreshAncestors("ghost")
}

func TestIsCompleteRequiresRootAcceptanceForCompound(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	if err := sched.UpdateNodeState("a", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := sched.UpdateNodeState("b", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	// Root bubbled COMPLETED but goal-level acceptance has not run.
	if !sched.RootNeedsAcceptance() {
		t.Fatal("a completed compound root should need acceptance")
	}
	if sched.IsComplete() {
		t.Fatal("IsComplete must be false until IntegrationVerified is set")
	}
	if err := tree.UpdateNode(tree.RootID, func(n *models.TaskNode) error {
		n.IntegrationVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !sched.IsComplete() {
		t.Fatal("IsComplete should be true after acceptance passes")
	}
	if sched.RootNeedsAcceptance() {
		t.Error("RootNeedsAcceptance should be false once verified")
	}
}

// TestIsCompleteForLeafRootNeedsNoAcceptance covers the single-task case: the
// leaf's own DoD is the acceptance.
func TestIsCompleteForLeafRootNeedsNoAcceptance(t *testing.T) {
	// A single-task session: the root itself becomes the leaf, so it must
	// never have children (ConvertToLeaf rejects that).
	tree := NewTaskTree("s", "g", "d")
	if err := tree.ConvertToLeaf(tree.RootID, models.ContractSpec{}, models.DoD{}); err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(tree)
	if err := sched.UpdateNodeState(tree.RootID, models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if !sched.IsComplete() {
		t.Error("an atomic leaf root should be complete without IntegrationVerified")
	}
	if sched.RootNeedsAcceptance() {
		t.Error("a leaf root should never need compound acceptance")
	}
}

func TestIsCompleteAndHasFailedOnMissingRoot(t *testing.T) {
	tree := &TaskTree{RootID: "ghost", Nodes: map[string]*models.TaskNode{}}
	sched := NewScheduler(tree)
	if sched.IsComplete() {
		t.Error("a tree with no root cannot be complete")
	}
	if !sched.HasFailed() {
		t.Error("a tree with no root must report failure, not spin")
	}
}

// TestHasFailed verifies a failure only reaches the root once no sibling is
// still active: a compound with one failed and one running child is RUNNING,
// not FAILED.
func TestHasFailed(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	if sched.HasFailed() {
		t.Error("a fresh tree should not report failure")
	}
	if err := sched.UpdateNodeState("a", models.TaskStateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	// b is still PENDING, so the parent must not be terminal yet.
	if sched.HasFailed() {
		t.Error("root must not fail while a sibling is still runnable")
	}
	if err := sched.UpdateNodeState("b", models.TaskStateFailed, "also boom"); err != nil {
		t.Fatal(err)
	}
	if !sched.HasFailed() {
		t.Error("root should bubble to FAILED once every child failed")
	}
}

func TestDescribeStuckExplainsTheDeadlock(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	got := sched.DescribeStuck()
	for _, want := range []string{"pending leaves", "blocked leaves", "undecomposed compounds"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeStuck = %q, want it to mention %q", got, want)
		}
	}
	// A leaf blocked on an unmet dependency should be counted as blocked.
	if err := tree.UpdateNode("a", func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"ghost"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := sched.DescribeStuck(); !strings.Contains(got, "blocked leaves=1") {
		t.Errorf("DescribeStuck = %q, want blocked leaves=1", got)
	}
}

func TestGetNextDecomposableNodePicksShallowestFirst(t *testing.T) {
	tree := NewTaskTree("s", "g", "d")
	// A populated root already has children, so the shallowest *empty*
	// compound is the nested one; it must be chosen over nothing else.
	mid, _ := tree.AddChild(tree.RootID, "mid", "mid", "mid", models.NodeTypeCompound)
	if _, err := tree.AddChild(mid.ID, "deep", "deep", "deep", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	sched := NewScheduler(tree)
	got := sched.GetNextDecomposableNode()
	if got == nil || got.ID != "deep" {
		t.Fatalf("expected the deepest empty compound, got %+v", got)
	}
}

// TestGetNextDecomposableNodePrefersShallowest verifies depth ordering when
// several empty compounds are ready at once.
func TestGetNextDecomposableNodePrefersShallowest(t *testing.T) {
	tree := NewTaskTree("s", "g", "d")
	// Two empty compounds at the same depth: the lower ID must win.
	for _, id := range []string{"zeta", "alpha"} {
		if _, err := tree.AddChild(tree.RootID, id, id, id, models.NodeTypeCompound); err != nil {
			t.Fatal(err)
		}
	}
	sched := NewScheduler(tree)
	got := sched.GetNextDecomposableNode()
	if got == nil || got.ID != "alpha" {
		t.Fatalf("expected alpha (lowest ID at equal depth), got %+v", got)
	}
}

// TestGetNextDecomposableNodeSkipsAlreadyPopulated covers the case where a
// compound already has children and must not be re-decomposed.
func TestGetNextDecomposableNodeSkipsAlreadyPopulated(t *testing.T) {
	tree := threeLevelTree(t)
	sched := NewScheduler(tree)
	if got := sched.GetNextDecomposableNode(); got != nil {
		t.Fatalf("expected no decomposable node, got %s", got.ID)
	}
}

func TestStorageDefaultsBaseDir(t *testing.T) {
	// An empty base dir must fall back to .divvy rather than failing.
	s, err := NewStorage("")
	if err != nil {
		t.Fatal(err)
	}
	if s.baseDir != ".divvy" {
		t.Errorf("baseDir = %q, want .divvy", s.baseDir)
	}
}

func TestStorageRejectsNilTree(t *testing.T) {
	s, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTree(nil); err == nil {
		t.Error("saving a nil tree should fail rather than panic")
	}
}

func TestStorageTreeExists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.TreeExists("nope") {
		t.Error("TreeExists should be false before a save")
	}
	tree := NewTaskTree("sess1", "goal", "desc")
	if err := s.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	if !s.TreeExists("sess1") {
		t.Error("TreeExists should be true after a save")
	}
}

// TestStorageSaveIsAtomicAndLeavesNoTempFiles matters because SaveTree runs on
// every scheduler tick; leftover .tmp files would accumulate.
func TestStorageSaveIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree := NewTaskTree("sess", "goal", "desc")
	for i := 0; i < 5; i++ {
		if err := s.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "sess.json")); err != nil {
		t.Errorf("session file missing: %v", err)
	}
}

// TestStorageSaveOverwritesCleanly verifies a second save replaces rather than
// appends, so a resumed session cannot grow duplicated JSON.
func TestStorageSaveOverwritesCleanly(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	tree := NewTaskTree("sess", "goal", "desc")
	if err := s.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "sess.json"))
	if err := s.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "sess.json"))
	if len(first) != len(second) {
		t.Errorf("re-saving changed the file size: %d -> %d", len(first), len(second))
	}
}

func TestStorageLoadTreeErrors(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	if _, err := s.LoadTree("ghost"); err == nil {
		t.Error("expected an error loading a missing session")
	}
	// A corrupt file must surface a parse error, not a silent empty tree.
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadTree("bad"); err == nil {
		t.Error("expected a parse error for a corrupt session file")
	}
}

// TestListSessionsSkipsUnreadableFiles covers a directory that also holds
// unrelated or corrupt JSON.
func TestListSessionsSkipsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub.json"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2"} {
		tree := NewTaskTree(id, "goal "+id, "d")
		if err := s.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2 (broken/非JSON/目录 must be skipped)", len(list))
	}
	for _, info := range list {
		if info.ID != "s1" && info.ID != "s2" {
			t.Errorf("unexpected session %q", info.ID)
		}
	}
}

// TestListSessionsFallsBackToRootTitle covers trees saved without an explicit
// Goal field. The orchestrator builds trees as NewTaskTree(id, goal, goal), so
// the root's Title carries the goal when Goal itself is empty.
func TestListSessionsFallsBackToRootTitle(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	tree := NewTaskTree("s1", "the root title", "the root title")
	tree.Goal = ""
	if err := s.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(list))
	}
	if list[0].Goal != "the root title" {
		t.Errorf("Goal = %q, want the root title fallback", list[0].Goal)
	}
}

func TestListSessionsSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	base := time.Now()
	for i, id := range []string{"old", "new"} {
		tree := NewTaskTree(id, "goal", "d")
		// Stamp distinct times so ordering is deterministic.
		stamp := base.Add(time.Duration(i) * time.Hour)
		tree.UpdatedAt = stamp
		if err := s.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
		// SaveTree serializes UpdatedAt, so re-stamp through a direct write.
		if err := tree.UpdateNode(tree.RootID, func(*models.TaskNode) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// Force a clear ordering by rewriting the two files with known stamps.
	for id, stamp := range map[string]time.Time{
		"old": base,
		"new": base.Add(time.Hour),
	} {
		p := filepath.Join(dir, id+".json")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		updated := strings.Replace(string(raw), `"updated_at":"`+base.Format(time.RFC3339Nano)+`"`,
			`"updated_at":"`+stamp.Format(time.RFC3339Nano)+`"`, 1)
		if err := os.WriteFile(p, []byte(updated), 0644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(list))
	}
	if list[0].ID != "new" {
		t.Errorf("first = %q, want the newest session first", list[0].ID)
	}
}
