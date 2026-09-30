package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/cost"
	"github.com/esrrhs/go_llm_engine/pkg/models"
)

// TaskTree coordinates the hierarchical structure of task nodes.
type TaskTree struct {
	ID        string                      `json:"id"`
	Goal      string                      `json:"goal,omitempty"`
	WorkDir   string                      `json:"work_dir,omitempty"`
	RootID    string                      `json:"root_id"`
	Nodes     map[string]*models.TaskNode `json:"nodes"`
	CreatedAt time.Time                   `json:"created_at"`
	UpdatedAt time.Time                   `json:"updated_at"`

	// ModelName is the model used by the session; drives per-node cost labels.
	ModelName string `json:"model_name,omitempty"`
	// PriceFor resolves ModelName to a token price; when set the tree view
	// annotates each node with its estimated USD cost. Not serialized.
	PriceFor func(model string) (cost.Price, bool) `json:"-"`

	mu sync.RWMutex
}

// NewTaskTree creates a new tree with a root compound node.
func NewTaskTree(id, rootTitle, rootDesc string) *TaskTree {
	rootID := "root"
	rootNode := models.NewTaskNode(rootID, "", rootTitle, rootDesc, models.NodeTypeCompound, 0)

	tree := &TaskTree{
		ID:        id,
		RootID:    rootID,
		Nodes:     make(map[string]*models.TaskNode),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	tree.Nodes[rootID] = rootNode
	return tree
}

// AddNode adds an existing node to the tree and links it to its parent.
func (t *TaskTree) AddNode(node *models.TaskNode) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.Nodes[node.ID]; exists {
		return fmt.Errorf("node with id %s already exists", node.ID)
	}

	if node.ParentID != "" {
		parent, exists := t.Nodes[node.ParentID]
		if !exists {
			return fmt.Errorf("parent node %s does not exist", node.ParentID)
		}
		parent.ChildrenIDs = append(parent.ChildrenIDs, node.ID)
		parent.UpdatedAt = time.Now()
	}

	t.Nodes[node.ID] = node
	t.UpdatedAt = time.Now()
	return nil
}

// AddChild creates a new child node under parentID and attaches it.
func (t *TaskTree) AddChild(parentID, childID, title, desc string, nodeType models.NodeType) (*models.TaskNode, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, exists := t.Nodes[parentID]
	if !exists {
		return nil, fmt.Errorf("parent node %s does not exist", parentID)
	}

	if _, exists := t.Nodes[childID]; exists {
		return nil, fmt.Errorf("node %s already exists", childID)
	}

	child := models.NewTaskNode(childID, parentID, title, desc, nodeType, parent.Depth+1)
	parent.ChildrenIDs = append(parent.ChildrenIDs, childID)
	parent.UpdatedAt = time.Now()

	t.Nodes[childID] = child
	t.UpdatedAt = time.Now()
	return child, nil
}

// GetNode safely retrieves a node by ID. The pointer aliases the live node;
// prefer CloneNode when reading without intending to mutate under the tree lock.
func (t *TaskTree) GetNode(id string) (*models.TaskNode, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	node, exists := t.Nodes[id]
	return node, exists
}

// CloneNode returns a deep copy of a node.
func (t *TaskTree) CloneNode(id string) (*models.TaskNode, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	node, exists := t.Nodes[id]
	if !exists {
		return nil, false
	}
	return node.Clone(), true
}

// GetRoot returns the root task node.
func (t *TaskTree) GetRoot() *models.TaskNode {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.Nodes[t.RootID]
}

// GetChildren returns all direct child nodes of given parentID.
func (t *TaskTree) GetChildren(parentID string) ([]*models.TaskNode, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	parent, exists := t.Nodes[parentID]
	if !exists {
		return nil, fmt.Errorf("node %s does not exist", parentID)
	}

	children := make([]*models.TaskNode, 0, len(parent.ChildrenIDs))
	for _, cid := range parent.ChildrenIDs {
		if child, ok := t.Nodes[cid]; ok {
			children = append(children, child)
		}
	}
	return children, nil
}

// GetProgress returns (completedCount, totalCount, percentage)
func (t *TaskTree) GetProgress() (int, int, float64) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	total := len(t.Nodes)
	if total == 0 {
		return 0, 0, 0.0
	}

	completed := 0
	for _, node := range t.Nodes {
		if node.State == models.TaskStateCompleted {
			completed++
		}
	}

	percent := (float64(completed) / float64(total)) * 100.0
	return completed, total, percent
}

// GetLeafProgress counts only leaf nodes.
func (t *TaskTree) GetLeafProgress() (int, int, float64) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	total := 0
	completed := 0
	for _, node := range t.Nodes {
		if node.Type != models.NodeTypeLeaf {
			continue
		}
		total++
		if node.State == models.TaskStateCompleted {
			completed++
		}
	}
	if total == 0 {
		return 0, 0, 0.0
	}
	return completed, total, (float64(completed) / float64(total)) * 100.0
}

// UpdateNode mutates a node under the tree lock.
func (t *TaskTree) UpdateNode(id string, fn func(*models.TaskNode) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, exists := t.Nodes[id]
	if !exists {
		return fmt.Errorf("node %s does not exist", id)
	}
	if err := fn(node); err != nil {
		return err
	}
	node.UpdatedAt = time.Now()
	t.UpdatedAt = time.Now()
	return nil
}

// HasNode reports whether an ID is already used.
func (t *TaskTree) HasNode(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, exists := t.Nodes[id]
	return exists
}

// NodeIDsInStates returns IDs whose state is one of the provided states.
func (t *TaskTree) NodeIDsInStates(states ...models.TaskState) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	want := make(map[models.TaskState]bool, len(states))
	for _, st := range states {
		want[st] = true
	}
	ids := make([]string, 0)
	for id, n := range t.Nodes {
		if want[n.State] {
			ids = append(ids, id)
		}
	}
	return ids
}

// Leaves returns clones of all leaf nodes, ordered by ID.
func (t *TaskTree) Leaves() []*models.TaskNode {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]*models.TaskNode, 0)
	for _, n := range t.Nodes {
		if n.Type == models.NodeTypeLeaf {
			out = append(out, n.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TotalTokenUsage sums the token usage saved on every node.
func (t *TaskTree) TotalTokenUsage() models.TokenUsage {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var out models.TokenUsage
	for _, n := range t.Nodes {
		out.Calls += n.TokenUsage.Calls
		out.PromptTokens += n.TokenUsage.PromptTokens
		out.CompletionTokens += n.TokenUsage.CompletionTokens
		out.TotalTokens += n.TokenUsage.TotalTokens
	}
	return out
}
func (t *TaskTree) ConvertToLeaf(id string, contract models.ContractSpec, dod models.DoD) error {
	return t.UpdateNode(id, func(n *models.TaskNode) error {
		if len(n.ChildrenIDs) > 0 {
			return fmt.Errorf("cannot convert node %s with children to leaf", id)
		}
		n.Type = models.NodeTypeLeaf
		n.State = models.TaskStatePending
		if len(contract.Inputs) > 0 || len(contract.Outputs) > 0 || len(contract.Constraints) > 0 || len(contract.Dependencies) > 0 {
			n.Contract = contract
		}
		if dod.Description != "" || len(dod.Commands) > 0 {
			n.DoD = dod
			if n.DoD.TimeoutSec <= 0 {
				n.DoD.TimeoutSec = 60
			}
		}
		return nil
	})
}

// ResetChildren deletes all descendants of parentID and returns the parent to PENDING compound.
func (t *TaskTree) ResetChildren(parentID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, exists := t.Nodes[parentID]
	if !exists {
		return fmt.Errorf("node %s does not exist", parentID)
	}

	var remove func(id string)
	remove = func(id string) {
		n := t.Nodes[id]
		if n == nil {
			return
		}
		for _, cid := range n.ChildrenIDs {
			remove(cid)
		}
		delete(t.Nodes, id)
	}
	for _, cid := range parent.ChildrenIDs {
		remove(cid)
	}
	parent.ChildrenIDs = make([]string, 0)
	parent.Type = models.NodeTypeCompound
	parent.State = models.TaskStatePending
	parent.UpdatedAt = time.Now()
	t.UpdatedAt = time.Now()
	return nil
}

// RenderVisualTree produces a clean CLI tree visualization.
func (t *TaskTree) RenderVisualTree() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var sb strings.Builder
	root, ok := t.Nodes[t.RootID]
	if !ok {
		return "[Empty Tree]"
	}

	t.renderNodeRecursive(&sb, root, "", true)
	return sb.String()
}

func (t *TaskTree) renderNodeRecursive(sb *strings.Builder, node *models.TaskNode, prefix string, isLast bool) {
	marker := "├── "
	if isLast {
		marker = "└── "
	}
	var tokens string
	if node.TokenUsage.Calls > 0 {
		tokens = fmt.Sprintf(" [%s tok]", formatTokens(node.TokenUsage.TotalTokens))
		if c, ok := t.nodeCost(node); ok {
			tokens += " " + cost.FormatUSD(c)
		}
	}
	if node.ID == t.RootID {
		sb.WriteString(fmt.Sprintf("[%s] %s (Root: %s)%s\n", node.State, node.Title, node.ID, tokens))
	} else {
		sb.WriteString(fmt.Sprintf("%s%s[%s] %s (%s: %s)%s\n", prefix, marker, node.State, node.Title, node.Type, node.ID, tokens))
	}

	childPrefix := prefix
	if node.ID != t.RootID {
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}
	}

	numChildren := len(node.ChildrenIDs)
	for i, cid := range node.ChildrenIDs {
		child, ok := t.Nodes[cid]
		if ok {
			t.renderNodeRecursive(sb, child, childPrefix, i == numChildren-1)
		}
	}
}

// nodeCost estimates a node's USD cost from its token usage and the session
// model price. Returns ok=false when no price is known for the model.
func (t *TaskTree) nodeCost(node *models.TaskNode) (float64, bool) {
	if t.PriceFor == nil || t.ModelName == "" {
		return 0, false
	}
	price, ok := t.PriceFor(t.ModelName)
	if !ok {
		return 0, false
	}
	c := price.Cost(node.TokenUsage.PromptTokens, node.TokenUsage.CompletionTokens)
	return c, true
}

// formatTokens renders a token count compactly for the tree view.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ToJSON serializes the task tree to JSON.
func (t *TaskTree) ToJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return json.MarshalIndent(t, "", "  ")
}

// FromJSON deserializes JSON bytes into a TaskTree.
func FromJSON(data []byte) (*TaskTree, error) {
	var tree TaskTree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	if tree.Nodes == nil {
		tree.Nodes = make(map[string]*models.TaskNode)
	}
	if tree.RootID == "" {
		tree.RootID = "root"
	}
	return &tree, nil
}
