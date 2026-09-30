package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/models"
)

// Scheduler governs the lifecycle, dependency resolution, and execution sequence of the TaskTree.
type Scheduler struct {
	tree *TaskTree
}

// NewScheduler creates a scheduler for the given tree.
func NewScheduler(tree *TaskTree) *Scheduler {
	return &Scheduler{tree: tree}
}

// GetTree returns the managed TaskTree.
func (s *Scheduler) GetTree() *TaskTree {
	return s.tree
}

// ValidateDependencies verifies that there are no missing or circular dependencies in the tree.
func (s *Scheduler) ValidateDependencies() error {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	for _, node := range s.tree.Nodes {
		for _, depID := range node.Contract.Dependencies {
			if _, exists := s.tree.Nodes[depID]; !exists {
				return fmt.Errorf("node %s references non-existent dependency %s", node.ID, depID)
			}
		}
	}

	visited := make(map[string]int) // 0: unvisited, 1: visiting, 2: visited

	var dfs func(id string) error
	dfs = func(id string) error {
		visited[id] = 1
		node := s.tree.Nodes[id]
		if node == nil {
			return fmt.Errorf("missing node %s during cycle detection", id)
		}
		for _, depID := range node.Contract.Dependencies {
			if visited[depID] == 1 {
				return fmt.Errorf("circular dependency detected involving %s and %s", id, depID)
			}
			if visited[depID] == 0 {
				if err := dfs(depID); err != nil {
					return err
				}
			}
		}
		visited[id] = 2
		return nil
	}

	for id := range s.tree.Nodes {
		if visited[id] == 0 {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}

	return nil
}

// AreDependenciesSatisfied checks if all dependencies of the node are in COMPLETED state.
func (s *Scheduler) AreDependenciesSatisfied(node *models.TaskNode) bool {
	if node == nil {
		return false
	}
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()
	return s.areDependenciesSatisfiedLocked(node)
}

func (s *Scheduler) areDependenciesSatisfiedLocked(node *models.TaskNode) bool {
	for _, depID := range node.Contract.Dependencies {
		depNode, exists := s.tree.Nodes[depID]
		if !exists || depNode.State != models.TaskStateCompleted {
			return false
		}
	}
	return true
}

// GetNextDecomposableNode finds a pending compound node ready to be broken down.
// The returned node is a clone and is safe to read without holding the tree lock.
func (s *Scheduler) GetNextDecomposableNode() *models.TaskNode {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	var best *models.TaskNode
	for _, node := range s.tree.Nodes {
		if node.Type != models.NodeTypeCompound || node.State != models.TaskStatePending {
			continue
		}
		if len(node.ChildrenIDs) != 0 {
			continue
		}
		if !s.areDependenciesSatisfiedLocked(node) {
			continue
		}
		if best == nil || node.Depth < best.Depth || (node.Depth == best.Depth && node.ID < best.ID) {
			best = node
		}
	}
	return best.Clone()
}

// GetReadyLeafNodes returns pending leaf nodes whose dependencies are satisfied.
// Nodes are clones, ordered by depth then ID for stable scheduling.
func (s *Scheduler) GetReadyLeafNodes() []*models.TaskNode {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	ready := make([]*models.TaskNode, 0)
	for _, node := range s.tree.Nodes {
		if node.Type == models.NodeTypeLeaf && node.State == models.TaskStatePending {
			if s.areDependenciesSatisfiedLocked(node) {
				ready = append(ready, node.Clone())
			}
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Depth != ready[j].Depth {
			return ready[i].Depth < ready[j].Depth
		}
		return ready[i].ID < ready[j].ID
	})
	return ready
}

// UpdateNodeState updates the state of a node and bubbles completion/failure to ancestors.
func (s *Scheduler) UpdateNodeState(nodeID string, newState models.TaskState, errorMsg string) error {
	s.tree.mu.Lock()
	node, exists := s.tree.Nodes[nodeID]
	if !exists {
		s.tree.mu.Unlock()
		return fmt.Errorf("node %s not found", nodeID)
	}

	node.State = newState
	node.ErrorMsg = errorMsg
	node.UpdatedAt = time.Now()
	parentID := node.ParentID
	s.tree.mu.Unlock()

	if parentID != "" {
		s.checkAndUpdateParent(parentID)
	}
	return nil
}

// RefreshAncestors re-evaluates parent state after a node type/state change that
// did not go through UpdateNodeState (for example converting a failed leaf into a compound).
func (s *Scheduler) RefreshAncestors(nodeID string) {
	s.tree.mu.RLock()
	node, exists := s.tree.Nodes[nodeID]
	var parentID string
	if exists {
		parentID = node.ParentID
	}
	s.tree.mu.RUnlock()
	if parentID != "" {
		s.checkAndUpdateParent(parentID)
	}
}

// checkAndUpdateParent walks ancestors synchronously and updates their aggregate state.
func (s *Scheduler) checkAndUpdateParent(parentID string) {
	for parentID != "" {
		s.tree.mu.Lock()
		parent, exists := s.tree.Nodes[parentID]
		if !exists || parent.Type != models.NodeTypeCompound || len(parent.ChildrenIDs) == 0 {
			s.tree.mu.Unlock()
			return
		}

		allSuccess := true
		hasFailed := false
		hasActive := false

		for _, cid := range parent.ChildrenIDs {
			child, ok := s.tree.Nodes[cid]
			if !ok {
				continue
			}
			switch child.State {
			case models.TaskStateCompleted, models.TaskStateSkipped:
			case models.TaskStateFailed:
				hasFailed = true
				allSuccess = false
			default:
				hasActive = true
				allSuccess = false
			}
		}

		switch {
		case allSuccess:
			parent.State = models.TaskStateCompleted
		case hasFailed && !hasActive:
			parent.State = models.TaskStateFailed
		default:
			parent.State = models.TaskStateRunning
		}
		parent.UpdatedAt = time.Now()
		next := parent.ParentID
		s.tree.mu.Unlock()
		parentID = next
	}
}

// IsComplete returns true only if the entire tree reached COMPLETED and, for
// a compound root, its goal-level acceptance DoD passed
// (IntegrationVerified). For an atomic leaf root the leaf's own DoD is the
// acceptance, so COMPLETED suffices. Children being done is not enough.
func (s *Scheduler) IsComplete() bool {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	root, exists := s.tree.Nodes[s.tree.RootID]
	if !exists {
		return false
	}
	if root.State != models.TaskStateCompleted {
		return false
	}
	return root.Type == models.NodeTypeLeaf || root.IntegrationVerified
}

// RootNeedsAcceptance reports whether a compound root's children all
// completed (root bubbled COMPLETED) but goal-level acceptance has not been
// verified yet.
func (s *Scheduler) RootNeedsAcceptance() bool {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()
	root, exists := s.tree.Nodes[s.tree.RootID]
	if !exists {
		return false
	}
	return root.Type == models.NodeTypeCompound &&
		root.State == models.TaskStateCompleted && !root.IntegrationVerified
}

// HasFailed returns true if the root is in a terminal FAILED state.
func (s *Scheduler) HasFailed() bool {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	root, exists := s.tree.Nodes[s.tree.RootID]
	if !exists {
		return true
	}
	return root.State == models.TaskStateFailed
}

// DescribeStuck explains why no node is currently runnable.
func (s *Scheduler) DescribeStuck() string {
	s.tree.mu.RLock()
	defer s.tree.mu.RUnlock()

	var pendingLeaves, blockedLeaves, pendingCompounds int
	for _, node := range s.tree.Nodes {
		switch {
		case node.Type == models.NodeTypeLeaf && node.State == models.TaskStatePending:
			pendingLeaves++
			if !s.areDependenciesSatisfiedLocked(node) {
				blockedLeaves++
			}
		case node.Type == models.NodeTypeCompound && node.State == models.TaskStatePending && len(node.ChildrenIDs) == 0:
			pendingCompounds++
		}
	}
	return fmt.Sprintf("pending leaves=%d blocked leaves=%d undecomposed compounds=%d", pendingLeaves, blockedLeaves, pendingCompounds)
}
