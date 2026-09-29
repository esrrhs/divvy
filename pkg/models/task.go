package models

import (
	"slices"
	"time"
)

// TokenUsage records LLM token consumption attributed to a node.
// It persists in the session tree so resumed sessions accumulate totals.
type TokenUsage struct {
	Calls            int `json:"calls,omitempty"`
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// TaskNode represents a single unit of goal/work in the decomposition tree.
type TaskNode struct {
	ID          string       `json:"id"`
	ParentID    string       `json:"parent_id,omitempty"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Type        NodeType     `json:"type"`
	State       TaskState    `json:"state"`
	Depth       int          `json:"depth"`
	ChildrenIDs []string     `json:"children_ids,omitempty"`
	Contract    ContractSpec `json:"contract"`
	DoD         DoD          `json:"dod"`

	// Execution & retry tracking
	RetryCount     int        `json:"retry_count"`
	MaxRetries     int        `json:"max_retries"`
	DecomposeCount int        `json:"decompose_count,omitempty"`
	ErrorMsg       string     `json:"error_msg,omitempty"`
	ResultSummary  string     `json:"result_summary,omitempty"`
	TokenUsage     TokenUsage `json:"token_usage"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// NewTaskNode initializes a new task node with sensible defaults.
func NewTaskNode(id, parentID, title, desc string, nodeType NodeType, depth int) *TaskNode {
	now := time.Now()
	return &TaskNode{
		ID:          id,
		ParentID:    parentID,
		Title:       title,
		Description: desc,
		Type:        nodeType,
		State:       TaskStatePending,
		Depth:       depth,
		ChildrenIDs: make([]string, 0),
		Contract: ContractSpec{
			Inputs:       make([]string, 0),
			Outputs:      make([]string, 0),
			Dependencies: make([]string, 0),
			Constraints:  make([]string, 0),
		},
		DoD: DoD{
			Commands:   make([]string, 0),
			TimeoutSec: 60,
		},
		MaxRetries: 0,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// IsLeaf returns true if node is a leaf task.
func (n *TaskNode) IsLeaf() bool {
	return n.Type == NodeTypeLeaf
}

// CanRetry returns whether node can still be retried. MaxRetries <= 0 means unlimited.
func (n *TaskNode) CanRetry() bool {
	if n.MaxRetries <= 0 {
		return true
	}
	return n.RetryCount < n.MaxRetries
}

// Clone returns a deep copy of the node (slices duplicated, safe to read without the tree lock).
func (n *TaskNode) Clone() *TaskNode {
	if n == nil {
		return nil
	}
	cp := *n
	cp.ChildrenIDs = slices.Clone(n.ChildrenIDs)
	cp.Contract.Inputs = slices.Clone(n.Contract.Inputs)
	cp.Contract.Outputs = slices.Clone(n.Contract.Outputs)
	cp.Contract.Dependencies = slices.Clone(n.Contract.Dependencies)
	cp.Contract.Constraints = slices.Clone(n.Contract.Constraints)
	cp.DoD.Commands = slices.Clone(n.DoD.Commands)
	return &cp
}
