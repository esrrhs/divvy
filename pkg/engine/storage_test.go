package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
)

func TestStorage_SaveAndLoad(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "divvy_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to init storage: %v", err)
	}

	sessionID := "test_session_123"
	tree := NewTaskTree(sessionID, "Root Task", "Root Description")
	_, err = tree.AddChild("root", "child_1", "Sub Task 1", "Sub desc", models.NodeTypeLeaf)
	if err != nil {
		t.Fatalf("failed to add child: %v", err)
	}

	// Save
	if err := storage.SaveTree(tree); err != nil {
		t.Fatalf("failed to save tree: %v", err)
	}

	// Check file exists
	expectedPath := filepath.Join(tempDir, sessionID+".json")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("expected file %s does not exist", expectedPath)
	}

	// Load
	loadedTree, err := storage.LoadTree(sessionID)
	if err != nil {
		t.Fatalf("failed to load tree: %v", err)
	}

	if loadedTree.ID != sessionID || len(loadedTree.Nodes) != 2 {
		t.Fatalf("loaded tree data mismatch: ID=%s, nodes count=%d", loadedTree.ID, len(loadedTree.Nodes))
	}

	child, exists := loadedTree.GetNode("child_1")
	if !exists || child.Title != "Sub Task 1" {
		t.Fatalf("child node not restored properly")
	}
}
