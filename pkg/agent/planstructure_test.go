package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// The regression this file guards: a runnable goal whose goal-level acceptance
// names a path ("go run ./cmd/demo") that no leaf produces. Every leaf can pass
// and the run still dies, because acceptance has no integration leaf to route
// its failure to. Observed on a real 66-leaf run that burned 16.7M tokens
// and then failed with "no integration leaf exists to fix assembly".

// deepPackagesNoEntryPlan looks like the real planner output: deep packages, no entry.
const deepPackagesNoEntryPlan = `{
  "is_atomic": false,
  "reason": "split by package",
  "dod": {"commands": ["go test ./...", "go run ./cmd/demo"]},
  "subtasks": [
    {"id": "vec2", "title": "Vec2", "description": "vector", "type": "LEAF",
     "contract": {"outputs": ["internal/core/core.go"]}, "dod": {"commands": ["go test ./..."]}},
    {"id": "world", "title": "World", "description": "world", "type": "LEAF",
     "contract": {"outputs": ["internal/store/store.go"]}, "dod": {"commands": ["go test ./..."]}}
  ]
}`

func TestGoalCommandPaths(t *testing.T) {
	cases := []struct {
		name string
		cmds []string
		want []string
	}{
		{"go run package", []string{"go run ./cmd/demo"}, []string{"./cmd/demo"}},
		{"go run file", []string{"go run cmd/demo/main.go"}, []string{"cmd/demo/main.go"}},
		{"go build dir", []string{"go build ./cmd/server"}, []string{"./cmd/server"}},
		{"direct exec", []string{"./bin/app --selftest"}, []string{"./bin/app"}},
		{"python", []string{"python3 app.py"}, []string{"app.py"}},
		{"node", []string{"node server.js"}, []string{"server.js"}},
		{"env prefix", []string{"GOFLAGS=-mod=mod go run ./cmd/demo"}, []string{"./cmd/demo"}},
		// -o names the artifact, not a package: the package still has to exist.
		{"go build -o", []string{"go build -o bin/app ./cmd/server"}, []string{"./cmd/server"}},
		{"go build -o=", []string{"go build -o=bin/app ./cmd/server"}, []string{"./cmd/server"}},
		{"windows exec", []string{`.\\bin\\app.exe`}, []string{`.\\bin\\app.exe`}},
		{"shell script", []string{"bash run.sh"}, []string{"run.sh"}},
		// Package scopes describe a set, not a path: reporting them would make
		// every plan that runs `go test ./...` look broken.
		{"module root", []string{"go build .", "go build ./..."}, nil},
		{"package pattern", []string{"go test ./..."}, nil},
		{"wildcard", []string{"go run ./cmd/*"}, nil},
		{"nothing", []string{"npm test --if-present", "cargo build"}, nil},
		{"python module flag", []string{"python3 -m pytest"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := goalCommandPaths(c.cmds)
			if len(got) != len(c.want) {
				t.Fatalf("goalCommandPaths(%v) = %v, want %v", c.cmds, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("goalCommandPaths(%v) = %v, want %v", c.cmds, got, c.want)
				}
			}
		})
	}
}

func TestPathCoveredByOutputs(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		outputs []string
		want    bool
	}{
		{"exact file", "app.py", []string{"app.py"}, true},
		{"file in declared dir", "./cmd/demo", []string{"cmd/demo/main.go"}, true},
		{"declared dir covers file", "cmd/demo/main.go", []string{"cmd/demo"}, true},
		{"normalized forms", "./cmd//demo", []string{"cmd/demo/main.go"}, true},
		{"sibling package does not cover", "./cmd/demo", []string{"cmd/other/main.go"}, false},
		{"same basename elsewhere does not cover", "cmd/demo", []string{"main.go"}, false},
		{"empty target is trivially covered", ".", []string{"x.go"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pathCoveredByOutputs(c.target, c.outputs); got != c.want {
				t.Fatalf("pathCoveredByOutputs(%q, %v) = %v, want %v",
					c.target, c.outputs, got, c.want)
			}
		})
	}
}

// structureFixture builds an orchestrator with a runnable goal, a go.mod, and
// a root that already has the given leaf outputs plus a goal-level DoD.
func structureFixture(t *testing.T, goal string, acceptance []string, leafOutputs map[string][]string) *Orchestrator {
	t.Helper()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module myapp\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_structure"
	cfg.Goal = goal
	cfg.Stream = false
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{
		Handle: func(context.Context, llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: `{"is_atomic":true,"subtasks":[]}`}, nil
		},
	}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: acceptance, TimeoutSec: 60}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for id, outs := range leafOutputs {
		if _, err := o.tree.AddChild(o.tree.RootID, id, id, "done", models.NodeTypeLeaf); err != nil {
			t.Fatal(err)
		}
		if err := o.tree.UpdateNode(id, func(n *models.TaskNode) error {
			n.Contract.Outputs = outs
			n.State = models.TaskStateCompleted
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPlanBlockers_CatchesUnbackedAcceptancePath(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go test ./...", "go run ./cmd/demo"},
		map[string][]string{
			"vec2":  {"internal/core/core.go"},
			"world": {"internal/store/store.go"},
		})

	blockers := o.planBlockers()
	if len(blockers) != 1 {
		t.Fatalf("want 1 blocker for the unproduced cmd/demo, got %d: %v", len(blockers), blockers)
	}
	if !strings.Contains(blockers[0], "cmd/demo") {
		t.Fatalf("blocker should name the path: %s", blockers[0])
	}

	// A leaf that produces the entry point clears it.
	if _, err := o.tree.AddChild(o.tree.RootID, "demo", "cmd/demo", "entry", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("demo", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"cmd/demo/main.go"}
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := o.planBlockers(); len(got) != 0 {
		t.Fatalf("covered path must not block, got %v", got)
	}
}

func TestPlanBlockers_DoesNotGuessFromGoalWording(t *testing.T) {
	// The goal does not sound runnable ("fix the parser bug"), but the root
	// acceptance still executes ./cmd/demo. That run cannot pass, so the gap
	// must be caught regardless of how the goal is phrased.
	o := structureFixture(t, "fix the parser bug", []string{"go run ./cmd/demo"},
		map[string][]string{"a": {"a.go"}})
	blockers := o.planBlockers()
	if len(blockers) != 1 || !strings.Contains(blockers[0], "cmd/demo") {
		t.Fatalf("concrete acceptance path must block regardless of goal wording, got %v", blockers)
	}
}

func TestPlanBlockers_IgnoresPackageScopedAcceptance(t *testing.T) {
	o := structureFixture(t, "写一个程序", []string{"go build .", "go build ./...", "go test ./..."},
		map[string][]string{"a": {"a.go"}})
	if got := o.planBlockers(); len(got) != 0 {
		t.Fatalf("package-scoped acceptance names no path and must not block, got %v", got)
	}
}

func TestEnsureIntegrationLeaf_AppendsAssemblyLeaf(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go test ./...", "go run ./cmd/demo"},
		map[string][]string{"vec2": {"internal/core/core.go"}, "world": {"internal/store/store.go"}})

	root, _ := o.tree.CloneNode(o.tree.RootID)
	id, err := o.ensureIntegrationLeaf("goal acceptance references \"cmd/demo\"")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no leaf id returned")
	}
	leaf, ok := o.tree.CloneNode(id)
	if !ok {
		t.Fatalf("leaf %s missing", id)
	}
	if leaf.Type != models.NodeTypeLeaf {
		t.Fatalf("type = %s, want leaf", leaf.Type)
	}
	// The injected leaf must produce exactly what acceptance runs, otherwise
	// the blocker it was created to fix is still open.
	wantOut := "cmd/demo/main.go"
	if len(leaf.Contract.Outputs) != 1 || leaf.Contract.Outputs[0] != wantOut {
		t.Fatalf("outputs = %v, want [%s]", leaf.Contract.Outputs, wantOut)
	}
	// It must run last: depend on every pre-existing sibling.
	for _, cid := range root.ChildrenIDs {
		found := false
		for _, d := range leaf.Contract.Dependencies {
			if d == cid {
				found = true
			}
		}
		if !found {
			t.Fatalf("leaf must depend on %s; deps = %v", cid, leaf.Contract.Dependencies)
		}
	}
	if len(leaf.DoD.Commands) != len(root.DoD.Commands) {
		t.Fatalf("injected leaf should inherit goal acceptance %v, got %v",
			root.DoD.Commands, leaf.DoD.Commands)
	}
	// Root must be runnable again, or the new PENDING leaf never executes.
	if got, _ := o.tree.CloneNode(o.tree.RootID); got.State != models.TaskStateRunning {
		t.Fatalf("root state = %s, want running", got.State)
	}
	if got := o.planBlockers(); len(got) != 0 {
		t.Fatalf("blocker must be resolved after injection, got %v", got)
	}
}

func TestEnsureIntegrationLeaf_IsIdempotent(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go run ./cmd/demo"},
		map[string][]string{"a": {"a.go"}})
	first, err := o.ensureIntegrationLeaf("gap")
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.ensureIntegrationLeaf("gap")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("second call created another leaf: %s vs %s", first, second)
	}
	// It should also reuse a leaf the planner already produced.
	o2 := structureFixture(t, "写一个可运行的程序", []string{"go run ./cmd/demo"},
		map[string][]string{"main": {"cmd/demo/main.go"}})
	id, err := o2.ensureIntegrationLeaf("gap")
	if err != nil {
		t.Fatal(err)
	}
	if id != "main" {
		t.Fatalf("expected existing entry leaf to be reused, got %s", id)
	}
}

// The regression end to end: a plan that never mentions cmd/demo is
// rejected at plan time, replanned once, then healed by injection.
func TestDecompose_ReplansThenInjectsEntryLeaf(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module myapp\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			calls++
			return &llm.Response{Content: deepPackagesNoEntryPlan}, nil
		},
	}
	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_structure_replan"
	cfg.Goal = "写一个可运行的程序"
	cfg.Stream = false
	cfg.MaxRedecompose = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunPlan(context.Background()); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// One rejected plan + one replan, then the budget is spent and the engine
	// appends the missing leaf itself instead of failing the run.
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2 (initial + one replan)", calls)
	}
	if !o.hasIntegrationLeaf() {
		t.Fatal("no integration leaf after replan budget was spent")
	}
	if got := o.planBlockers(); len(got) != 0 {
		t.Fatalf("plan still blocked: %v", got)
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if root.State == models.TaskStateFailed {
		t.Fatal("root must not be failed: the whole point is to keep the run recoverable")
	}
}

// A tree saved by an older build (root already FAILED, no assembly leaf) must
// become resumable again instead of exiting non-zero forever.
func TestHealPlanStructure_RevivesFailedRoot(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go test ./...", "go run ./cmd/demo"},
		map[string][]string{"vec2": {"internal/core/core.go"}})
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.State = models.TaskStateFailed
		n.ErrorMsg = "goal-level acceptance failed and no integration leaf exists to fix assembly"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	o.healPlanStructure()

	root, _ := o.tree.CloneNode(o.tree.RootID)
	if root.State != models.TaskStateRunning {
		t.Fatalf("root state = %s, want running after heal", root.State)
	}
	if !o.hasIntegrationLeaf() {
		t.Fatal("heal did not add an assembly leaf")
	}
}

// A failed child is a different problem; healing must not pretend to fix it
// (the injected leaf would depend on a node that never completes).
func TestHealPlanStructure_SkipsWhenChildFailed(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go run ./cmd/demo"},
		map[string][]string{"vec2": {"internal/core/core.go"}})
	if err := o.tree.UpdateNode("vec2", func(n *models.TaskNode) error {
		n.State = models.TaskStateFailed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	o.healPlanStructure()
	if o.hasIntegrationLeaf() {
		t.Fatal("must not inject an assembly leaf on top of a failed child")
	}
}

// A real plan contained a leaf named "integrate_collision_resolution"
// (it integrated one package's modules). Treating a name match as the assembly
// leaf swallowed the detected gap and the run still died.
func TestEnsureIntegrationLeaf_IgnoresPackageIntegrator(t *testing.T) {
	o := structureFixture(t, "写一个可运行的程序", []string{"go run ./cmd/demo"},
		map[string][]string{"vec2": {"internal/core/core.go"}})
	if _, err := o.tree.AddChild(o.tree.RootID, "collision", "Collision", "d", models.NodeTypeCompound); err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild("collision", "integrate_collision_resolution",
		"Integrate collision resolution", "d", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("integrate_collision_resolution", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"internal/core/wire.go"}
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if id := o.findIntegrationLeaf(); id != "" {
		t.Fatalf("nested package integrator %q must not count as the assembly leaf", id)
	}
	id, err := o.ensureIntegrationLeaf("gap")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := o.tree.CloneNode(id)
	if leaf == nil || leaf.Contract.Outputs[0] != "cmd/demo/main.go" {
		t.Fatalf("expected a real assembly leaf producing cmd/demo/main.go, got %+v", leaf)
	}
}

// The full loop: a session that already died with "no integration leaf exists
// to fix assembly" must, on resume, grow the missing leaf, run it, and pass the
// goal-level acceptance instead of exiting 1 again.
func TestHealPlanStructure_ResumeCompletesTheGoal(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module myapp\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "internal", "core"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "internal", "core", "core.go"), []byte("package core\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_structure_resume"
	cfg.Goal = "写一个可运行的程序"
	cfg.Stream = false
	cfg.MaxRetries = 1
	cfg.MaxStall = 2

	// Phase 1: the tree the failing run left behind — all leaves done, root
	// failed because acceptance had nowhere to go.
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{
		Handle: func(context.Context, llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: `{"is_atomic":true,"subtasks":[]}`}, nil
		},
	}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild(o.tree.RootID, "vec2", "Vec2", "done", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode("vec2", func(n *models.TaskNode) error {
		n.Contract.Outputs = []string{"internal/core/core.go"}
		n.State = models.TaskStateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: []string{"go run ./cmd/demo"}, TimeoutSec: 60}
		n.State = models.TaskStateFailed
		n.ErrorMsg = "goal-level acceptance failed and no integration leaf exists to fix assembly"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: resume. The worker writes exactly the entry point acceptance
	// runs; nothing else should be needed.
	worker := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: `{"is_atomic":true,"subtasks":[]}`}, nil
			}
			if reqHasToolResult(req) {
				return jsonAction("finish", map[string]string{"summary": "wired entry"}), nil
			}
			return jsonAction("write_file", map[string]string{
				"path":    "cmd/demo/main.go",
				"content": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"demo ok\")\n}\n",
			}), nil
		},
	}
	o2, err := Load(cfg, worker, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o2.Run(context.Background()); err != nil {
		t.Fatalf("resume must complete after healing, got: %v", err)
	}

	root, _ := o2.tree.CloneNode(o2.tree.RootID)
	if root.State != models.TaskStateCompleted {
		t.Fatalf("root state = %s, want completed", root.State)
	}
	if !root.IntegrationVerified {
		t.Fatal("goal-level acceptance was never verified")
	}
	if _, err := os.Stat(filepath.Join(work, "cmd", "demo", "main.go")); err != nil {
		t.Fatalf("entry point was not created: %v", err)
	}
}
