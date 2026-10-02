package engine

import (
	"os"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
)

func TestStorage_ListSessions(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Build two sessions with distinct goals.
	tree1 := NewTaskTree("sess_one", "first goal", "first goal")
	if _, err := tree1.AddChild("root", "leaf_a", "Leaf A", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveTree(tree1); err != nil {
		t.Fatal(err)
	}

	tree2 := NewTaskTree("sess_two", "second goal", "second goal")
	if _, err := tree2.AddChild("root", "leaf_b", "Leaf B", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := tree2.UpdateNode("leaf_b", func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveTree(tree2); err != nil {
		t.Fatal(err)
	}

	// A non-tree JSON file must be skipped rather than abort the listing.
	if err := os.WriteFile(storage.baseDir+"/junk.json", []byte("{not a tree"), 0644); err != nil {
		t.Fatal(err)
	}

	sessions, err := storage.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (junk skipped)", len(sessions))
	}

	byID := map[string]SessionInfo{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	s1 := byID["sess_one"]
	if s1.Goal != "first goal" || s1.LeavesAll != 1 || s1.LeavesDone != 0 {
		t.Fatalf("sess_one summary wrong: %+v", s1)
	}
	if s1.RootState != models.TaskStatePending {
		t.Fatalf("root state: %s", s1.RootState)
	}
	s2 := byID["sess_two"]
	if s2.LeavesDone != 1 || s2.LeavesAll != 1 {
		t.Fatalf("sess_two summary wrong: %+v", s2)
	}
	if s2.RootState != models.TaskStatePending {
		t.Fatalf("root not touched by scheduler, want PENDING, got %s", s2.RootState)
	}
}

func TestStorage_ListSessionsEmpty(t *testing.T) {
	storage, _ := NewStorage(t.TempDir())
	sessions, err := storage.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected empty, got %d", len(sessions))
	}
}
