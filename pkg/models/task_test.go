package models

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTaskStateIsTerminal(t *testing.T) {
	terminal := map[TaskState]bool{
		TaskStateCompleted: true,
		TaskStateFailed:    true,
		TaskStateSkipped:   true,
		TaskStatePending:   false,
		TaskStateRunning:   false,
		TaskStateVerifying: false,
		// DECOMPOSING is easy to forget; it is explicitly not terminal.
		TaskStateDecomposing: false,
	}
	for state, want := range terminal {
		if got := state.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", state, got, want)
		}
	}
}

func TestNewTaskNodeDefaults(t *testing.T) {
	n := NewTaskNode("n1", "root", "title", "desc", NodeTypeLeaf, 2)

	if n.ID != "n1" || n.ParentID != "root" {
		t.Fatalf("identity wrong: %+v", n)
	}
	if n.State != TaskStatePending {
		t.Errorf("State = %s, want PENDING", n.State)
	}
	if n.Depth != 2 {
		t.Errorf("Depth = %d, want 2", n.Depth)
	}
	// Slices must be non-nil so JSON encodes them as [] rather than null and
	// so appends never panic on a nil-backed field.
	if n.ChildrenIDs == nil || n.Contract.Outputs == nil || n.DoD.Commands == nil {
		t.Error("slices should be initialized, not nil")
	}
	if n.DoD.TimeoutSec != 60 {
		t.Errorf("DoD.TimeoutSec = %d, want 60", n.DoD.TimeoutSec)
	}
	if n.CreatedAt.IsZero() || n.UpdatedAt.IsZero() {
		t.Error("timestamps should be set")
	}
	if !n.IsLeaf() {
		t.Error("a leaf node should report IsLeaf")
	}
}

func TestIsLeaf(t *testing.T) {
	if !(&TaskNode{Type: NodeTypeLeaf}).IsLeaf() {
		t.Error("LEAF should be a leaf")
	}
	if (&TaskNode{Type: NodeTypeCompound}).IsLeaf() {
		t.Error("COMPOUND should not be a leaf")
	}
}

func TestCanRetry(t *testing.T) {
	cases := []struct {
		name       string
		maxRetries int
		retryCount int
		want       bool
	}{
		{"unlimited always retries", 0, 0, true},
		{"unlimited retries many times", 0, 99, true},
		{"negative is treated as unlimited", -1, 5, true},
		{"under the limit", 3, 2, true},
		{"at the limit", 3, 3, false},
		{"over the limit", 3, 4, false},
	}
	for _, tc := range cases {
		n := &TaskNode{MaxRetries: tc.maxRetries, RetryCount: tc.retryCount}
		if got := n.CanRetry(); got != tc.want {
			t.Errorf("%s: CanRetry() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCloneIsDeep guards the invariant the whole engine relies on: callers read
// clones without holding the tree lock, so a shallow copy would let them
// mutate live state and race.
func TestCloneIsDeep(t *testing.T) {
	orig := NewTaskNode("n1", "root", "t", "d", NodeTypeCompound, 1)
	orig.ChildrenIDs = append(orig.ChildrenIDs, "c1")
	orig.Contract.Inputs = []string{"in"}
	orig.Contract.Outputs = []string{"out"}
	orig.Contract.Dependencies = []string{"dep"}
	orig.Contract.Constraints = []string{"con"}
	orig.DoD.Commands = []string{"go test ./..."}
	orig.ErrorHistory = []ErrorRecord{{Error: "boom"}}

	cp := orig.Clone()

	// Mutating every slice field on the clone must not touch the original.
	cp.ChildrenIDs[0] = "MUTATED"
	cp.Contract.Inputs[0] = "MUTATED"
	cp.Contract.Outputs[0] = "MUTATED"
	cp.Contract.Dependencies[0] = "MUTATED"
	cp.Contract.Constraints[0] = "MUTATED"
	cp.DoD.Commands[0] = "MUTATED"
	cp.ErrorHistory[0].Error = "MUTATED"
	cp.Title = "MUTATED"

	if orig.ChildrenIDs[0] != "c1" {
		t.Error("ChildrenIDs is shared with the clone")
	}
	if orig.Contract.Inputs[0] != "in" {
		t.Error("Contract.Inputs is shared with the clone")
	}
	if orig.Contract.Outputs[0] != "out" {
		t.Error("Contract.Outputs is shared with the clone")
	}
	if orig.Contract.Dependencies[0] != "dep" {
		t.Error("Contract.Dependencies is shared with the clone")
	}
	if orig.Contract.Constraints[0] != "con" {
		t.Error("Contract.Constraints is shared with the clone")
	}
	if orig.DoD.Commands[0] != "go test ./..." {
		t.Error("DoD.Commands is shared with the clone")
	}
	if orig.ErrorHistory[0].Error != "boom" {
		t.Error("ErrorHistory is shared with the clone")
	}
	if orig.Title != "t" {
		t.Error("Title should be an independent copy")
	}
}

func TestCloneNilReceiver(t *testing.T) {
	var n *TaskNode
	if n.Clone() != nil {
		t.Fatal("cloning a nil node should return nil")
	}
}

func TestTokenUsageJSONRoundTrip(t *testing.T) {
	u := TokenUsage{Calls: 3, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var back TokenUsage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != u {
		t.Fatalf("round trip changed the value: %+v -> %+v", u, back)
	}
}

func TestTaskNodeJSONRoundTripPreservesContract(t *testing.T) {
	n := NewTaskNode("leaf1", "root", "write parser", "desc", NodeTypeLeaf, 1)
	n.Contract = ContractSpec{
		Inputs:       []string{"a.go"},
		Outputs:      []string{"b.go"},
		Dependencies: []string{"other"},
		Constraints:  []string{"no new deps"},
	}
	n.DoD = DoD{Description: "it compiles", Commands: []string{"go build ./..."}, ExpectedOutput: "ok", TimeoutSec: 90}
	n.RetryCount = 2
	n.DecomposeCount = 1
	n.IntegrationVerified = true
	n.ResultSummary = "done"
	n.TokenUsage = TokenUsage{Calls: 4, TotalTokens: 999}
	n.ErrorHistory = []ErrorRecord{{Error: "first failure"}}

	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var back TaskNode
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.Contract, back.Contract) {
		t.Errorf("contract changed: %+v -> %+v", n.Contract, back.Contract)
	}
	if !reflect.DeepEqual(n.DoD, back.DoD) {
		t.Errorf("DoD changed: %+v -> %+v", n.DoD, back.DoD)
	}
	if back.RetryCount != 2 || back.DecomposeCount != 1 || !back.IntegrationVerified {
		t.Errorf("progress fields lost: %+v", back)
	}
	if back.ResultSummary != "done" {
		t.Errorf("ResultSummary = %q", back.ResultSummary)
	}
	if back.TokenUsage.TotalTokens != 999 {
		t.Errorf("TokenUsage = %+v", back.TokenUsage)
	}
	if len(back.ErrorHistory) != 1 || back.ErrorHistory[0].Error != "first failure" {
		t.Errorf("ErrorHistory = %+v", back.ErrorHistory)
	}
	if back.CreatedAt.IsZero() {
		t.Error("CreatedAt lost in round trip")
	}
}

// TestOmitEmptyKeepsTreeJSONSmall documents that unset optional fields are
// omitted, which keeps the persisted session file small.
func TestOmitEmptyKeepsTreeJSONSmall(t *testing.T) {
	n := NewTaskNode("n", "", "t", "d", NodeTypeLeaf, 0)
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"error_msg", "result_summary", "integration_verified", "parent_id"} {
		if _, ok := m[absent]; ok {
			t.Errorf("%q should be omitted when empty", absent)
		}
	}
}
