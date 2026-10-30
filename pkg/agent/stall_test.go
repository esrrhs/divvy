package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// TestStallFingerprintIsStableForTheSameFailure is the core property: two
// attempts that fail identically must hash the same, or the stall detector
// can never fire.
func TestStallFingerprintIsStableForTheSameFailure(t *testing.T) {
	a := stallFingerprint(tools.ProjectGo, "pkg/a.go:4:2: undefined: Foo\nFAIL\tpkg\t0.01s")
	b := stallFingerprint(tools.ProjectGo, "pkg/a.go:4:2: undefined: Foo\nFAIL\tpkg\t0.01s")
	if a == "" {
		t.Fatal("fingerprint must not be empty for a real failure")
	}
	if a != b {
		t.Fatalf("same failure produced different fingerprints: %q vs %q", a, b)
	}

	other := stallFingerprint(tools.ProjectGo, "pkg/b.go:9:2: undefined: Bar\nFAIL\tpkg\t0.01s")
	if other == a {
		t.Fatal("different failures must not share a fingerprint")
	}

	if got := stallFingerprint(tools.ProjectGo, "   "); got != "" {
		t.Fatalf("empty failure must yield an empty fingerprint, got %q", got)
	}
}

// TestStallFingerprintIgnoresVolatileCounts makes sure incidental numbers
// (attempt counters, timings, byte counts) do not defeat the comparison when
// no diagnostic rule applies: the failure is the same, only the count moved.
func TestStallFingerprintIgnoresVolatileCounts(t *testing.T) {
	a := stallFingerprint(tools.ProjectGeneric, "error: 12 widgets missing")
	b := stallFingerprint(tools.ProjectGeneric, "error: 13 widgets missing")
	if a == "" || a != b {
		t.Fatalf("numeric noise must not change the fingerprint: %q vs %q", a, b)
	}
}

// TestNodeStallRun covers the trailing-run counter, including the legacy
// records written before fingerprints existed (they must never count).
func TestNodeStallRun(t *testing.T) {
	n := &models.TaskNode{}
	if n.StallRun() != 0 {
		t.Fatal("no history means no stall")
	}
	n.ErrorHistory = []models.ErrorRecord{
		{Error: "old", Fingerprint: "x"},
		{Error: "new", Fingerprint: "y"},
		{Error: "new", Fingerprint: "y"},
	}
	if got := n.StallRun(); got != 2 {
		t.Fatalf("trailing run should be 2, got %d", got)
	}
	// A legacy record without a fingerprint ends the run with 0.
	n.ErrorHistory = []models.ErrorRecord{{Error: "legacy"}}
	if got := n.StallRun(); got != 0 {
		t.Fatalf("unfingerprinted records must not count as a stall, got %d", got)
	}
}

// TestOrchestrator_StalledThreshold reads the guard in isolation: the node's
// persisted history decides, and MaxStall=0 disables detection entirely.
func TestOrchestrator_StalledThreshold(t *testing.T) {
	tree := newStallTestTree("1.1", 3)
	o := &Orchestrator{cfg: Config{MaxStall: 3}, tree: tree, log: SilentLogger()}
	run, stalled := o.stalled("1.1")
	if run != 3 || !stalled {
		t.Fatalf("run=%d stalled=%v; want 3 true", run, stalled)
	}

	tree2 := newStallTestTree("1.1", 2)
	o2 := &Orchestrator{cfg: Config{MaxStall: 3}, tree: tree2, log: SilentLogger()}
	if _, stalled := o2.stalled("1.1"); stalled {
		t.Fatal("2 identical failures must not trip a limit of 3")
	}

	o3 := &Orchestrator{cfg: Config{MaxStall: 0}, tree: newStallTestTree("1.1", 9), log: SilentLogger()}
	if _, stalled := o3.stalled("1.1"); stalled {
		t.Fatal("MaxStall=0 must disable stall detection")
	}

	if _, stalled := (&Orchestrator{cfg: Config{MaxStall: 3}, tree: tree, log: SilentLogger()}).stalled("missing"); stalled {
		t.Fatal("unknown node must never be reported as stalled")
	}
}

// TestStallWithNoReplanKeepsRetrying: identical failures still raise
// leaf_stall, but with no replan budget left the leaf stays pending so the
// run does not exit.
func TestStallWithNoReplanKeepsRetrying(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_stall"
	cfg.Goal = "write a library file"
	cfg.Stream = false
	cfg.MaxRedecompose = 0

	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	leaf, _ := o.tree.CloneNode(o.tree.RootID)
	if err := o.handleStall(leaf, 3, "grep -n 'func TestX' vec_test.go\nexit 1"); err != nil {
		t.Fatal(err)
	}
	live, _ := o.tree.CloneNode(o.tree.RootID)
	if live.State != models.TaskStatePending || live.Type != models.NodeTypeLeaf {
		t.Fatalf("state=%s type=%s, want PENDING leaf", live.State, live.Type)
	}
	if len(live.ErrorHistory) != 0 {
		t.Fatalf("stall history should reset, got %d", len(live.ErrorHistory))
	}
	ev, err := os.ReadFile(o.EventPath())
	if err != nil || !strings.Contains(string(ev), "leaf_stall") {
		t.Fatalf("expected a leaf_stall event in %s (err=%v)", o.EventPath(), err)
	}
}

// TestOrchestrator_NoStallWhenFailuresDiffer is the other half of the
// contract: a leaf that keeps failing *differently* is still making progress
// and must be allowed to keep trying until MaxRetries runs out.
func TestOrchestrator_NoStallWhenFailuresDiffer(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var attempts int64
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: failingLeafPlan}, nil
			}
			attempts++
			return jsonAction("finish", map[string]string{"summary": "tried"}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_no_stall"
	cfg.Goal = "write a library file"
	cfg.Stream = false
	cfg.MaxStall = 0   // detection off: only MaxRetries can stop this
	cfg.MaxRetries = 3 // ...and it stops later than the stall limit would
	cfg.MaxRedecompose = 0
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = o.Run(ctx)
	if attempts < 3 {
		t.Fatalf("with stall detection off the leaf should keep retrying past MaxRetries, ran %d", attempts)
	}
	if ev, err := os.ReadFile(o.EventPath()); err == nil && strings.Contains(string(ev), "leaf_stall") {
		t.Fatal("no leaf_stall event should be recorded when detection is off")
	}
}

// newStallTestTree builds a one-leaf tree whose node carries n identical
// failures, i.e. a leaf that has been failing the same way n times.
func newStallTestTree(id string, n int) *engine.TaskTree {
	tree := engine.NewTaskTree("sess_stall", "goal", "goal")
	if _, err := tree.AddChild(tree.RootID, id, "leaf", "leaf", models.NodeTypeLeaf); err != nil {
		panic(err)
	}
	_ = tree.UpdateNode(id, func(node *models.TaskNode) error {
		for i := 0; i < n; i++ {
			node.ErrorHistory = append(node.ErrorHistory, models.ErrorRecord{
				Time:        time.Now(),
				Error:       "same failure",
				Fingerprint: "same-fp",
			})
		}
		return nil
	})
	return tree
}

func TestSoftOnlyDoD(t *testing.T) {
	if !softOnlyDoD([]string{"grep -F 'type X' a.go", "test -f a.go"}) {
		t.Fatal("grep+test -f should be soft-only")
	}
	if softOnlyDoD([]string{"go test ./..."}) {
		t.Fatal("go test must not be soft-only")
	}
	if softOnlyDoD([]string{"grep X a.go", "go build ./..."}) {
		t.Fatal("mixed with compile must not be soft-only")
	}
	if !hasCodeOutputs([]string{"physics/x.go", "README.md"}) {
		t.Fatal("expected code output detection")
	}
	if hasCodeOutputs([]string{"README.md", "docs/guide.md"}) {
		t.Fatal("docs-only should not count as code outputs")
	}
}
