package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// buildAcceptanceTree creates a compound root with two completed children:
// an implementation leaf and an integration leaf producing main.go.
func buildAcceptanceTree(t *testing.T, rootDoD models.DoD) *Orchestrator {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_accept"
	cfg.Goal = "build an http server API"
	cfg.Stream = false
	cfg.MaxRetries = 3

	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"impl", "integrate"} {
		if _, err := o.tree.AddChild("root", id, id, id, models.NodeTypeLeaf); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"main.go"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("root", func(n *models.TaskNode) error {
		n.DoD = rootDoD
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Mark children completed so the root bubbles COMPLETED.
	for _, id := range []string{"impl", "integrate"} {
		if err := o.sched.UpdateNodeState(id, models.TaskStateCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func TestRootAcceptance_RoutesFailureToIntegrationLeaf(t *testing.T) {
	// The end-to-end check fails (a file it needs is absent).
	dod := models.DoD{Commands: []string{"test -f definitely_missing_smoke"}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)

	if !o.sched.RootNeedsAcceptance() {
		t.Fatal("expected root to need acceptance")
	}
	if err := o.verifyRootAcceptance(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Failure is routed to the integration leaf; root is RUNNING again.
	integrate, _ := o.tree.CloneNode("integrate")
	if integrate.State != models.TaskStatePending {
		t.Fatalf("integrate state = %s, want PENDING", integrate.State)
	}
	if !strings.Contains(integrate.ErrorMsg, "goal acceptance failed") {
		t.Fatalf("missing routed error: %q", integrate.ErrorMsg)
	}
	root := o.tree.GetRoot()
	if root.State == models.TaskStateCompleted {
		t.Fatal("root must not stay COMPLETED after acceptance failure")
	}
	if o.sched.IsComplete() {
		t.Fatal("tree must not be complete")
	}
	if got := o.findIntegrationLeaf(); got != "integrate" {
		t.Fatalf("findIntegrationLeaf = %q", got)
	}
}

func TestRootAcceptance_PassesAndCompletes(t *testing.T) {
	// End-to-end check passes after the leaf fixes assembly.
	dod := models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)

	if err := o.verifyRootAcceptance(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := o.tree.GetRoot()
	if !root.IntegrationVerified {
		t.Fatal("root should be flagged IntegrationVerified")
	}
	if !o.sched.IsComplete() {
		t.Fatalf("tree should be complete:\n%s", o.tree.RenderVisualTree())
	}
	if o.sched.RootNeedsAcceptance() {
		t.Fatal("acceptance must not run twice")
	}
}

func TestRootAcceptance_FailsFatalWithoutIntegrationLeaf(t *testing.T) {
	// A tree with one ordinary leaf and no integration leaf: when the goal
	// acceptance fails there is nothing to self-heal → root FAILED.
	dod := models.DoD{Commands: []string{"test -f missing_x"}, TimeoutSec: 10}
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_accept_noleaf"
	cfg.Goal = "http service"
	o2, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o2.tree.AddChild("root", "plain", "plain", "plain", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o2.tree.UpdateNode("root", func(n *models.TaskNode) error {
		n.DoD = dod
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o2.sched.UpdateNodeState("plain", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := o2.verifyRootAcceptance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o2.sched.HasFailed() {
		t.Fatal("root should be FAILED when no integration leaf exists")
	}
}

func TestGoalIsRunnable(t *testing.T) {
	runnable := []string{
		"用 Go 写一个 HTTP 服务",
		"build a CLI tool",
		"开发一个 API 服务端，监听端口",
	}
	notRunnable := []string{
		"给 Add 函数写单元测试",
		"修复 storage 里的一个 bug",
	}
	for _, g := range runnable {
		if !goalIsRunnable(g) {
			t.Fatalf("expected runnable: %q", g)
		}
	}
	for _, g := range notRunnable {
		if goalIsRunnable(g) {
			t.Fatalf("expected not runnable: %q", g)
		}
	}
}

func TestPlanWarnings_IntegrationGaps(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_pw_integration"
	cfg.Goal = "用 Go 写一个 HTTP 服务"
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild("root", "impl", "Impl", "impl", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("impl", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"lib.go"}
		n.DoD = models.DoD{Commands: []string{"go build ./..."}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	warns := o.planWarnings()
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "entry point") {
		t.Fatalf("expected missing-entry warning:\n%s", joined)
	}
	if !strings.Contains(joined, "goal-level acceptance") {
		t.Fatalf("expected missing goal-acceptance warning:\n%s", joined)
	}
}
