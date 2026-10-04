package engine

import (
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
)

func TestTaskTree_Operations(t *testing.T) {
	tree := NewTaskTree("sess_1", "Build Operating System", "A toy OS in Go")
	if tree.RootID != "root" {
		t.Fatalf("expected root ID 'root', got %s", tree.RootID)
	}

	root := tree.GetRoot()
	if root == nil || root.Title != "Build Operating System" {
		t.Fatalf("root node invalid")
	}

	// Add child node
	c1, err := tree.AddChild("root", "task_mem", "Memory Management", "Implement paging", models.NodeTypeCompound)
	if err != nil {
		t.Fatalf("failed to add child: %v", err)
	}
	if c1.ParentID != "root" || c1.Depth != 1 {
		t.Fatalf("child node parent or depth wrong")
	}

	// Add leaf node under c1
	leaf1, err := tree.AddChild("task_mem", "task_mem_bitmap", "Bitmap Allocator", "Implement simple bitmap", models.NodeTypeLeaf)
	if err != nil {
		t.Fatalf("failed to add leaf: %v", err)
	}
	if !leaf1.IsLeaf() {
		t.Fatalf("expected leaf node")
	}

	// Progress
	comp, total, pct := tree.GetProgress()
	if total != 3 || comp != 0 || pct != 0.0 {
		t.Fatalf("unexpected initial progress: comp=%d total=%d pct=%f", comp, total, pct)
	}

	// Update leaf to completed
	leaf1.State = models.TaskStateCompleted
	comp, total, pct = tree.GetProgress()
	if comp != 1 || total != 3 || pct <= 0 {
		t.Fatalf("progress after completion wrong: comp=%d total=%d pct=%f", comp, total, pct)
	}

	// Visual tree render
	vis := tree.RenderVisualTree()
	if !strings.Contains(vis, "Build Operating System") || !strings.Contains(vis, "task_mem_bitmap") {
		t.Fatalf("render visual tree missing content: %s", vis)
	}
}
