package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// wavePlan is a nested plan: two compounds, two leaves each. Nested on purpose —
// a single flat decomposition has nothing left to expand, so it cannot show the
// throttle at work.
const wavePlanRoot = `{
  "is_atomic": false,
  "reason": "two groups",
  "dod": {"commands": ["true"]},
  "subtasks": [
    {"id": "group_a", "title": "Group A", "description": "first group", "type": "compound",
     "contract": {"outputs": []}, "dod": {"commands": ["true"]}},
    {"id": "group_b", "title": "Group B", "description": "second group", "type": "compound",
     "contract": {"outputs": []}, "dod": {"commands": ["true"]}}
  ]
}`

func wavePlanGroup(id string) string {
	b, _ := json.Marshal(map[string]any{
		"is_atomic": false,
		"reason":    "two leaves",
		"dod":       map[string]any{"commands": []string{"true"}},
		"subtasks": []map[string]any{
			{"id": id + "_1", "title": id + " one", "description": "leaf one", "type": "leaf",
				"contract": map[string]any{"outputs": []string{id + "_1.txt"}},
				"dod":      map[string]any{"commands": []string{"true"}}},
			{"id": id + "_2", "title": id + " two", "description": "leaf two", "type": "leaf",
				"contract": map[string]any{"outputs": []string{id + "_2.txt"}},
				"dod":      map[string]any{"commands": []string{"true"}}},
		},
	})
	return string(b)
}

// taskIDFromDecomposePrompt reads the "Task ID:" line the decomposer is given.
func taskIDFromDecomposePrompt(req llm.Request) string {
	for _, m := range req.Messages {
		for _, line := range strings.Split(m.Content, "\n") {
			if strings.HasPrefix(line, "Task ID: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "Task ID: "))
			}
		}
	}
	return ""
}

// wroteAlready reports whether this worker has already written something.
//
// The signal has to come from the transcript, not from a sentinel the worker
// invents: the executor appends "Tool write_file result:" after every write, and
// a spy that never finishes simply burns MaxSteps and fails the leaf, which
// looks like a deadlock from the outside.
func wroteAlready(req llm.Request) bool {
	return strings.Contains(joinedContents(req.Messages), "Tool write_file result:")
}


// TestWaveHasRoom_CapsTheFrontier pins the throttle itself: the tree stops
// growing while the wave still holds unexecuted leaves, and grows anyway when
// nothing can run (otherwise the run would deadlock).
func TestWaveHasRoom_CapsTheFrontier(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, nil)
	o.cfg.Wave = 2
	for i := 0; i < 2; i++ {
		if _, err := o.tree.AddChild(o.tree.RootID, "leaf"+string(rune('a'+i)), "leaf", "d", models.NodeTypeLeaf); err != nil {
			t.Fatal(err)
		}
	}
	if got := o.tree.PendingLeafCount(); got != 2 {
		t.Fatalf("pending leaves = %d, want 2", got)
	}
	if o.waveHasRoom(false) {
		t.Error("the tree grew past the wave size")
	}
	if !o.waveHasRoom(true) {
		t.Error("nothing was runnable, so decomposition had to be allowed")
	}
	o.cfg.Wave = 0
	if !o.waveHasRoom(false) {
		t.Error("-wave 0 must not throttle")
	}
}

// TestRun_WaveThrottleDefersDecomposition is the behavioural claim of the
// refactor: the planner does not get to think about group B until group A's
// leaves have produced code.
func TestRun_WaveThrottleDefersDecomposition(t *testing.T) {
	run := func(t *testing.T, wave int) (leavesBeforeB int64, done bool) {
		t.Helper()
		var finished atomic.Int64
		var atB atomic.Int64
		atB.Store(-1)

		client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				switch id := taskIDFromDecomposePrompt(req); id {
				case o_rootID:
					return &llm.Response{Content: wavePlanRoot}, nil
				case "group_a", "group_b":
					if id == "group_b" {
						// Freeze the frontier at the moment the planner first
						// thinks about group B: that is the whole claim.
						atB.Store(finished.Load())
					}
					return &llm.Response{Content: wavePlanGroup(id)}, nil
				default:
					return &llm.Response{Content: `{"is_atomic":true,"dod":{"commands":["true"]}}`}, nil
				}
			}
			// Worker: write the file this leaf declared, then finish.
			if wroteAlready(req) {
				finished.Add(1)
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			}
			id := taskIDFromDecomposePrompt(req)
			return jsonAction("write_file", map[string]string{"path": id + ".txt", "content": "x\n"}), nil
		}}

		cfg := DefaultConfig()
		cfg.WorkDir = t.TempDir()
		cfg.DataDir = t.TempDir()
		cfg.SessionID = "test_wave"
		cfg.Goal = "two groups"
		cfg.Stream = false
		cfg.MaxSteps = 6
		cfg.Wave = wave
		cfg.RetryMinInterval = time.Millisecond
		cfg.RetryMaxInterval = 2 * time.Millisecond

		o, err := NewFromGoal(cfg, client, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = o.Run(ctx)
		return atB.Load(), err == nil
	}

	t.Run("throttled", func(t *testing.T) {
		leaves, ok := run(t, 2)
		if !ok {
			t.Fatal("run did not complete")
		}
		if leaves < 1 {
			t.Fatalf("group B was decomposed before any leaf landed (%d leaves finished) — the wave did not throttle", leaves)
		}
	})

	t.Run("flat", func(t *testing.T) {
		leaves, ok := run(t, 0)
		if !ok {
			t.Fatal("run did not complete")
		}
		if leaves != 0 {
			t.Fatalf("-wave 0 should plan both groups up front, but group B waited for %d leaves", leaves)
		}
	})
}

// o_rootID is the root node id used by NewFromGoal ("root").
const o_rootID = "root"

// TestRun_WaveBatchesExecution pins the half of the throttle that planning
// alone cannot deliver.
//
// A real run showed why: the root decomposition emitted all six leaves of the
// goal in one shot, so capping *decomposition* changed nothing and the first
// aggregate still ran after every leaf — the old "assemble once at the end"
// shape. The wave has to bound execution too, or a flat plan is unaffected.
func TestRun_WaveBatchesExecution(t *testing.T) {
	leaves := `{
  "is_atomic": false,
  "reason": "flat plan of five",
  "dod": {"commands": ["true"]},
  "subtasks": [
    {"id": "l1", "title": "one", "description": "d", "type": "leaf",
     "contract": {"outputs": ["l1.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "l2", "title": "two", "description": "d", "type": "leaf",
     "contract": {"outputs": ["l2.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "l3", "title": "three", "description": "d", "type": "leaf",
     "contract": {"outputs": ["l3.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "l4", "title": "four", "description": "d", "type": "leaf",
     "contract": {"outputs": ["l4.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "l5", "title": "five", "description": "d", "type": "leaf",
     "contract": {"outputs": ["l5.txt"]}, "dod": {"commands": ["true"]}}
  ]
}`
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, architectMarker) {
			return &llm.Response{Content: leaves}, nil
		}
		if wroteAlready(req) {
			return jsonAction("finish", map[string]string{"summary": "done"}), nil
		}
		return jsonAction("write_file", map[string]string{"path": taskIDFromDecomposePrompt(req) + ".txt", "content": "x\n"}), nil
	}}

	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_wave_batch"
	cfg.Goal = "five leaves"
	cfg.Stream = false
	cfg.MaxSteps = 6
	cfg.Wave = 2
	// Parallelism is what the launch quota has to fight: with a serial pool the
	// pool itself would hand out one leaf per pass and hide a missing quota.
	cfg.Parallel = 4
	cfg.AggregateCommands = []string{"true"}
	cfg.RetryMinInterval = time.Millisecond
	cfg.RetryMaxInterval = 2 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev, err := SummarizeEvents(o.EventPath())
	if err != nil {
		t.Fatal(err)
	}
	// Five leaves at two per wave: 2 + 2 + 1, so three aggregates.
	if ev.Aggregates < 3 {
		t.Fatalf("aggregates = %d, want at least 3 (5 leaves at 2 per wave)", ev.Aggregates)
	}
	if ev.Aggregates > 6 {
		t.Fatalf("aggregates = %d; the wave is not batching execution", ev.Aggregates)
	}
}

// TestPlanWarnings_IgnoresRepairLeaves keeps the injected repair task out of the
// plan-quality review. It declares no outputs on purpose — it edits files that
// already exist — so flagging it as "weak" would be warning about the fix.
func TestPlanWarnings_IgnoresRepairLeaves(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, nil)
	defer o.Close()
	if _, err := o.tree.AddChild(o.tree.RootID, "weak", "no outputs on purpose", "", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := o.tree.AddChild(o.tree.RootID, waveRepairPrefix+"_1", "Repair the workspace", "", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	var sawRepair, sawWeak bool
	for _, w := range o.planWarnings() {
		if strings.Contains(w, waveRepairPrefix) {
			sawRepair = true
		}
		if strings.HasPrefix(w, "weak ") {
			sawWeak = true
		}
	}
	if sawRepair {
		t.Error("the repair leaf was reported as a weak contract")
	}
	if !sawWeak {
		t.Error("an ordinary leaf with no outputs should still be reported")
	}
}

// TestAggregateReady_DerivedCommandNeedsSources pins the difference between the
// operator's -aggregate (a contract, always runs) and a derived one (only once
// there is code to check).
//
// The case that matters is a greenfield Go module: "go build ./..." exits 0
// there while "go test ./..." exits 1 with "no packages to test", so a derived
// aggregate would report a brand-new project as broken and spend a whole wave
// repairing code nobody has written yet. The same failure made the baseline
// suppress every repair in the run, so this rule is load-bearing twice.
func TestAggregateReady_DerivedCommandNeedsSources(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newOrch := func(cfg Config) *Orchestrator {
		t.Helper()
		cfg.WorkDir = work
		cfg.SessionID = "test_aggregate_ready"
		cfg.Goal = "build package x"
		o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		return o
	}

	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	o := newOrch(cfg)
	if got := o.projectType(); got != tools.ProjectGo {
		t.Fatalf("project type = %q, want Go", got)
	}
	if len(o.aggregateCommands()) == 0 {
		t.Fatal("a Go workspace should still offer commands")
	}
	if o.aggregateReady() {
		t.Error("a derived aggregate must wait until the module has a package")
	}

	// With one package present the derived check is meaningful again.
	if err := os.WriteFile(filepath.Join(work, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !o.aggregateReady() {
		t.Error("with a package present the derived aggregate should run")
	}

	// An explicit -aggregate is the operator's contract, sources or not.
	cfg2 := DefaultConfig()
	cfg2.DataDir = t.TempDir()
	cfg2.AggregateCommands = []string{"true"}
	if !newOrch(cfg2).aggregateReady() {
		t.Error("explicit -aggregate must always run")
	}
}

// TestRun_AggregateFailureQueuesRepairLeaf is the end-to-end claim: a wave that
// leaves the workspace broken does not get buried under more waves. The failure
// becomes the next wave's first task, and the run only finishes once the
// workspace is whole again.
func TestRun_AggregateFailureQueuesRepairLeaf(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{Content: `{
  "is_atomic": false,
  "reason": "two leaves",
  "dod": {"commands": ["test -f leaf.txt"]},
  "subtasks": [
    {"id": "write_a", "title": "write a", "description": "write the file", "type": "leaf",
     "contract": {"outputs": ["leaf.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "write_b", "title": "write b", "description": "write the file again", "type": "leaf",
     "contract": {"outputs": ["leaf.txt"]}, "dod": {"commands": ["true"]}}
  ]
}`}, nil
		}
		whole := joinedContents(req.Messages)
		if wroteAlready(req) {
			return jsonAction("finish", map[string]string{"summary": "done"}), nil
		}
		// The repair leaf is told to fix the regression; everyone else just
		// writes the ordinary file.
		if strings.Contains(whole, "Repair the regression") {
			return jsonAction("write_file", map[string]string{"path": "wave-ok.txt", "content": "ok\n"}), nil
		}
		return jsonAction("write_file", map[string]string{"path": "leaf.txt", "content": "x\n"}), nil
	}}

	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_wave_repair"
	cfg.Goal = "write leaf.txt"
	cfg.Stream = false
	cfg.MaxSteps = 6
	cfg.Wave = 3
	// The aggregate is satisfied only once the repair leaf has done its job.
	cfg.AggregateCommands = []string{"test -f wave-ok.txt"}
	cfg.RetryMinInterval = time.Millisecond
	cfg.RetryMaxInterval = 2 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A repair leaf was created and completed.
	var repair *models.TaskNode
	for _, leaf := range o.tree.Leaves() {
		if strings.HasPrefix(leaf.ID, waveRepairPrefix) {
			repair = leaf
		}
	}
	if repair == nil {
		t.Fatal("no repair leaf was queued for the broken aggregate")
	}
	if repair.State != models.TaskStateCompleted {
		t.Fatalf("repair leaf state = %s", repair.State)
	}
	if _, err := os.Stat(filepath.Join(o.sandbox.Root, "wave-ok.txt")); err != nil {
		t.Fatalf("the repair leaf's file is missing: %v", err)
	}

	// The aggregate ran at least twice and failed the first time.
	ev, err := SummarizeEvents(o.EventPath())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Counts["wave_aggregate"] < 2 {
		t.Fatalf("wave_aggregate events = %d, want at least 2", ev.Counts["wave_aggregate"])
	}
	if ev.Counts["wave_repair"] != 1 {
		t.Fatalf("wave_repair events = %d, want 1", ev.Counts["wave_repair"])
	}
}

// TestRun_WaveZeroSkipsAggregate keeps -wave 0 an honest control group: the old
// flat behaviour must not grow an aggregate check on the way.
func TestRun_WaveZeroSkipsAggregate(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, architectMarker) {
			return &llm.Response{Content: `{
  "is_atomic": false,
  "reason": "one leaf",
  "dod": {"commands": ["test -f leaf.txt"]},
  "subtasks": [
    {"id": "write_a", "title": "write a", "description": "write the file", "type": "leaf",
     "contract": {"outputs": ["leaf.txt"]}, "dod": {"commands": ["true"]}},
    {"id": "write_b", "title": "write b", "description": "write it too", "type": "leaf",
     "contract": {"outputs": ["leaf.txt"]}, "dod": {"commands": ["true"]}}
  ]
}`}, nil
		}
		if wroteAlready(req) {
			return jsonAction("finish", map[string]string{"summary": "done"}), nil
		}
		return jsonAction("write_file", map[string]string{"path": "leaf.txt", "content": "x\n"}), nil
	}}

	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_wave_off"
	cfg.Goal = "write leaf.txt"
	cfg.Stream = false
	cfg.MaxSteps = 6
	cfg.Wave = 0
	// Would fail every time if it ran at all.
	cfg.AggregateCommands = []string{"test -f never.txt"}
	cfg.RetryMinInterval = time.Millisecond
	cfg.RetryMaxInterval = 2 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev, err := SummarizeEvents(o.EventPath())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Counts["wave_aggregate"] != 0 {
		t.Fatalf("wave_aggregate ran %d times with -wave 0", ev.Counts["wave_aggregate"])
	}
	for _, leaf := range o.tree.Leaves() {
		if strings.HasPrefix(leaf.ID, waveRepairPrefix) {
			t.Fatal("a repair leaf was queued with -wave 0")
		}
	}
}
