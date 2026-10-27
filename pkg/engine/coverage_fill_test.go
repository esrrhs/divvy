package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
)

// --- scheduler edge branches -------------------------------------------------

// TestGetNextDecomposableNodeSkipsBlockedCompound covers the guard that keeps
// a pending compound whose dependencies are not satisfied out of the
// decomposition queue.
func TestGetNextDecomposableNodeSkipsBlockedCompound(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must2 := func(_ *models.TaskNode, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must2(tree.AddChild("root", "blocked", "Blocked", "", models.NodeTypeCompound))
	must2(tree.AddChild("root", "gate", "Gate", "", models.NodeTypeLeaf))
	// Make "blocked" depend on the pending leaf "gate".
	must(tree.UpdateNode("blocked", func(n *models.TaskNode) error {
		n.Contract.Dependencies = []string{"gate"}
		return nil
	}))

	s := NewScheduler(tree)
	if got := s.GetNextDecomposableNode(); got != nil {
		t.Fatalf("blocked compound must not be decomposable, got %s", got.ID)
	}
}

// TestGetReadyLeafNodesSortedByDepth covers the depth-first comparator of
// GetReadyLeafNodesAvoiding with leaves at different depths.
func TestGetReadyLeafNodesSortedByDepth(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	must2 := func(_ *models.TaskNode, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must2(tree.AddChild("root", "shallow", "Shallow", "", models.NodeTypeLeaf))
	must2(tree.AddChild("root", "mid", "Mid", "", models.NodeTypeCompound))
	must2(tree.AddChild("mid", "deep", "Deep", "", models.NodeTypeLeaf))

	s := NewScheduler(tree)
	ready := s.GetReadyLeafNodes()
	if len(ready) != 2 {
		t.Fatalf("want 2 ready leaves, got %d", len(ready))
	}
	if ready[0].ID != "shallow" || ready[1].ID != "deep" {
		t.Fatalf("order = %s,%s; shallow (depth 1) must precede deep (depth 2)", ready[0].ID, ready[1].ID)
	}
}

// TestOutputSetClaimsAllEmpty covers outputs that normalize to nothing: the
// claim set is nil rather than an empty map.
func TestOutputSetClaimsAllEmpty(t *testing.T) {
	got := OutputSetClaims([]string{"  ", "", "\t"})
	if got != nil {
		t.Fatalf("blank outputs must produce no claims, got %v", got)
	}
}

// TestUpdateNodeStateMissingParentStopsBubble covers checkAndUpdateParent
// returning early when the parent id does not exist.
func TestUpdateNodeStateMissingParentStopsBubble(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	node := models.NewTaskNode("lone", "ghost", "Lone", "", models.NodeTypeLeaf, 1)
	tree.Nodes["lone"] = node

	s := NewScheduler(tree)
	// Must not panic even though "ghost" is not in the tree.
	if err := s.UpdateNodeState("lone", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if node.State != models.TaskStateCompleted {
		t.Fatalf("state = %s", node.State)
	}
}

// TestCheckAndUpdateParentSkipsGhostChild covers the continue when a compound
// lists a child id that is not in the tree.
func TestCheckAndUpdateParentSkipsGhostChild(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	if _, err := tree.AddChild("root", "real", "Real", "", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	root := tree.GetRoot()
	root.ChildrenIDs = append(root.ChildrenIDs, "ghost")

	s := NewScheduler(tree)
	if err := s.UpdateNodeState("real", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if got := tree.GetRoot().State; got != models.TaskStateCompleted {
		t.Fatalf("root state = %s, want completed (ghost child ignored)", got)
	}
}

// TestIsCompleteRootStillPending covers the not-yet-completed root shortcut.
func TestIsCompleteRootStillPending(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	if NewScheduler(tree).IsComplete() {
		t.Fatal("a pending root is not complete")
	}
}

// TestRootNeedsAcceptanceMissingRoot covers the missing-root guard.
func TestRootNeedsAcceptanceMissingRoot(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	tree.RootID = "vanished"
	if NewScheduler(tree).RootNeedsAcceptance() {
		t.Fatal("a tree without a root cannot need acceptance")
	}
}

// TestDescribeStuckCountsUndecomposedCompound covers the pending-compound
// counter.
func TestDescribeStuckCountsUndecomposedCompound(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	got := NewScheduler(tree).DescribeStuck()
	if !strings.Contains(got, "undecomposed compounds=1") {
		t.Fatalf("DescribeStuck = %q, want one undecomposed compound", got)
	}
}

// --- storage edge branches ---------------------------------------------------

// TestNewStorageRejectsFilePath covers the MkdirAll failure when the base
// directory path is occupied by a regular file.
func TestNewStorageRejectsFilePath(t *testing.T) {
	tmp := t.TempDir()
	blocked := filepath.Join(tmp, "occupied")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStorage(filepath.Join(blocked, "sub")); err == nil {
		t.Fatal("expected an error when the base dir path is a file")
	}
}

// TestSaveTreeMkdirErrors covers the two directory-creation failure paths of
// SaveTree: the target dir cannot be created, and the temp file cannot be
// created inside it (tree ids containing separators).
func TestSaveTreeMkdirErrors(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewStorage(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// baseDir/sub is a file → MkdirAll(baseDir/sub) fails.
	if err := os.WriteFile(filepath.Join(tmp, "sub"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree1 := NewTaskTree("sub/x", "root", "root")
	if err := s.SaveTree(tree1); err == nil {
		t.Fatal("expected MkdirAll failure for separator-carrying id")
	}

	// baseDir/dir2 exists → MkdirAll succeeds, but the CreateTemp pattern
	// carries a separator pointing at a missing directory.
	if err := os.Mkdir(filepath.Join(tmp, "dir2"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree2 := NewTaskTree("dir2/y", "root", "root")
	if err := s.SaveTree(tree2); err == nil {
		t.Fatal("expected CreateTemp failure for separator-carrying id")
	}
}

// TestSaveTreeRenameOntoDirectory covers the atomic-rename failure when the
// destination path is a directory.
func TestSaveTreeRenameOntoDirectory(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewStorage(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmp, "d.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTree(NewTaskTree("d", "root", "root")); err == nil {
		t.Fatal("rename onto a directory must fail")
	}
}

// TestPlanPruneOnMissingBaseDir covers PlanPrune and ListSessions failing
// when the storage directory does not exist.
func TestPlanPruneOnMissingBaseDir(t *testing.T) {
	s := &Storage{baseDir: filepath.Join(t.TempDir(), "never-created")}
	if _, err := s.PlanPrune(1); err == nil {
		t.Fatal("PlanPrune must fail when the base dir is unreadable")
	}
	if _, err := s.ListSessions(); err == nil {
		t.Fatal("ListSessions must fail when the base dir is missing")
	}
}

// TestPruneSessionsRemoveError covers the per-file remove failure report:
// with a read-only base directory nothing can be deleted and the paths come
// back as kept.
func TestPruneSessionsRemoveError(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewStorage(tmp)
	if err != nil {
		t.Fatal(err)
	}
	old := NewTaskTree("sess_old", "root", "root")
	old.UpdatedAt = old.UpdatedAt.Add(-time.Hour)
	if err := s.SaveTree(old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTree(NewTaskTree("sess_new", "root", "root")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	plan, err := s.PruneSessions(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removed) != 1 || plan.Removed[0].ID != "sess_old" {
		t.Fatalf("plan.Removed = %+v", plan.Removed)
	}
	if len(plan.Removed[0].Paths) == 0 {
		t.Fatal("the tree file could not be removed and must be reported as kept")
	}
}

// TestDeleteSessionStatError covers a stat failure that is not NotExist.
func TestDeleteSessionStatError(t *testing.T) {
	tmp := t.TempDir()
	blocked := filepath.Join(tmp, "occupied")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Storage{baseDir: filepath.Join(blocked, "sub")}
	if _, err := s.DeleteSession("sess_x"); err == nil || os.IsNotExist(err) {
		t.Fatalf("want a non-NotExist stat error, got %v", err)
	}
}

// TestDeleteSessionRemoveErrors covers both remove-failure paths (tree file
// and LATEST pointer) when the storage directory is read-only.
func TestDeleteSessionRemoveErrors(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewStorage(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTree(NewTaskTree("sess_ro", "root", "root")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "LATEST"), []byte("sess_ro"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	art, err := s.DeleteSession("sess_ro")
	if err == nil {
		t.Fatal("expected the remove failures to surface")
	}
	if len(art.Paths) == 0 {
		t.Fatal("artifacts must still be reported")
	}
}

// TestLatestIDWithoutPointer covers latestID returning "" when no LATEST
// file exists.
func TestLatestIDWithoutPointer(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewStorage(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTree(NewTaskTree("sess_one", "root", "root")); err != nil {
		t.Fatal(err)
	}
	// No LATEST file was ever written.
	if got := s.latestID(); got != "" {
		t.Fatalf("latestID = %q, want empty", got)
	}
	plan, err := s.PlanPrune(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 1 || len(plan.Removed) != 0 {
		t.Fatalf("the only session must be kept: %+v", plan)
	}
}

// --- tree edge branches ------------------------------------------------------

// TestAddNodeLinksParent covers AddNode attaching to an existing parent.
func TestAddNodeLinksParent(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	child := models.NewTaskNode("adopted", "root", "Adopted", "", models.NodeTypeLeaf, 1)
	if err := tree.AddNode(child); err != nil {
		t.Fatal(err)
	}
	children, err := tree.GetChildren("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != "adopted" {
		t.Fatalf("children = %v", children)
	}
}

// TestGetProgressEmptyTree covers the zero-total shortcut.
func TestGetProgressEmptyTree(t *testing.T) {
	tree := &TaskTree{Nodes: map[string]*models.TaskNode{}}
	done, total, pct := tree.GetProgress()
	if done != 0 || total != 0 || pct != 0 {
		t.Fatalf("empty tree progress = %d/%d %v", done, total, pct)
	}
}

// TestResetChildrenToleratesGhostChild covers the nil-node guard inside the
// recursive removal.
func TestResetChildrenToleratesGhostChild(t *testing.T) {
	tree := NewTaskTree("t", "root", "root")
	if _, err := tree.AddChild("root", "real", "Real", "", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	tree.GetRoot().ChildrenIDs = append(tree.GetRoot().ChildrenIDs, "ghost")
	if err := tree.ResetChildren("root"); err != nil {
		t.Fatal(err)
	}
	if len(tree.Nodes) != 1 {
		t.Fatalf("only the root should remain, have %d nodes", len(tree.Nodes))
	}
}

// TestFromJSONEmptyDocument covers the nil-Nodes repair on an object without
// a nodes field.
func TestFromJSONEmptyDocument(t *testing.T) {
	tree, err := FromJSON([]byte(`{"goal":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	if tree.Nodes == nil {
		t.Fatal("Nodes must be repaired to an empty map")
	}
	if tree.RootID != "root" {
		t.Fatalf("RootID = %q, want the default", tree.RootID)
	}
}
