package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// baseTestConfig returns a config rooted at fresh temp dirs.
func baseTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_" + strings.ReplaceAll(t.Name(), "/", "_")
	cfg.Goal = "do a thing"
	cfg.Stream = false
	return cfg
}

func TestNew_ValidationErrors(t *testing.T) {
	// WorkDir is a file → sandbox creation fails.
	fileWork := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(fileWork, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig(t)
	cfg.WorkDir = fileWork
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("file workdir must fail NewSandbox")
	}

	// -web with a search template missing the {query} placeholder.
	cfg = baseTestConfig(t)
	cfg.WebEnabled = true
	cfg.SearchURL = "https://search.example/no-placeholder"
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "{query}") {
		t.Fatalf("bad search URL must fail, got %v", err)
	}

	// -git-commit outside a repository.
	cfg = baseTestConfig(t)
	cfg.GitCommit = true
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "git repository") {
		t.Fatalf("git commit outside repo must fail, got %v", err)
	}

	// Manual approval requires isolation.
	cfg = baseTestConfig(t)
	cfg.LeafApproval = "manual"
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "isolate") {
		t.Fatalf("manual approval without isolate must fail, got %v", err)
	}

	// DataDir is a file → storage creation fails.
	cfg = baseTestConfig(t)
	fileData := filepath.Join(t.TempDir(), "datablock")
	if err := os.WriteFile(fileData, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = fileData
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("file datadir must fail storage creation")
	}

	// Custom pricing pointing at a missing file.
	cfg = baseTestConfig(t)
	cfg.PricingJSON = filepath.Join(t.TempDir(), "no-such-pricing.json")
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "pricing") {
		t.Fatalf("missing pricing file must fail, got %v", err)
	}
}

func TestNewFromGoal_RequiresGoal(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Goal = ""
	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("empty goal must fail, got %v", err)
	}
}

func TestLoad_ErrorPaths(t *testing.T) {
	// No LATEST pointer and no session id.
	cfg := baseTestConfig(t)
	cfg.SessionID = ""
	if _, err := Load(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "LATEST") {
		t.Fatalf("missing LATEST must fail, got %v", err)
	}
	// LATEST names a session that does not exist.
	cfg2 := baseTestConfig(t)
	cfg2.SessionID = "ghost"
	if _, err := Load(cfg2, &llm.ScriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("unknown session must fail to load")
	}
}

func TestOrchestrator_SinksDegradeGracefully(t *testing.T) {
	// The log directory is a file: opening sinks must warn, not crash.
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "logs"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig(t)
	cfg.DataDir = data
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatalf("run must survive an unwritable log dir: %v", err)
	}
	o.Close()

	// The event stream path is a directory.
	data2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data2, "events", o.SessionID()+".jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg2 := baseTestConfig(t)
	cfg2.DataDir = data2
	o2, err := NewFromGoal(cfg2, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatalf("run must survive an unwritable event stream: %v", err)
	}
	if o2.treeBC != nil {
		t.Fatal("no broadcaster expected without an event stream")
	}
	o2.Close()
	o2.Close() // double Close is safe
}

func TestBytesTrim(t *testing.T) {
	for in, want := range map[string]string{
		"abc\n":     "abc",
		"abc\r\n":   "abc",
		"abc  ":     "abc",
		"\n\r \n":   "",
		"untouched": "untouched",
	} {
		if got := bytesTrim([]byte(in)); got != want {
			t.Errorf("bytesTrim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrchestrator_ResetInterrupted(t *testing.T) {
	cfg := baseTestConfig(t)
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	root := o.tree.RootID

	// A running root with an error message parks as pending with a reason.
	_ = o.tree.UpdateNode(root, func(n *models.TaskNode) error {
		n.State = models.TaskStateRunning
		n.ErrorMsg = "stale failure text"
		return nil
	})
	// A failed leaf gets a fresh error history.
	_ = o.tree.UpdateNode(root, func(n *models.TaskNode) error {
		n.State = models.TaskStateFailed
		n.Type = models.NodeTypeLeaf
		n.ErrorHistory = []models.ErrorRecord{{Error: "old"}}
		return nil
	})
	o.resetInterrupted()
	live, _ := o.tree.CloneNode(root)
	if live.State != models.TaskStatePending {
		t.Fatalf("failed leaf must be requeued, state=%s", live.State)
	}
	if len(live.ErrorHistory) != 0 {
		t.Fatalf("failed leaf must start with a clean history: %+v", live.ErrorHistory)
	}

	// A failed compound stays failed (it is re-decomposed, not retried).
	_ = o.tree.UpdateNode(root, func(n *models.TaskNode) error {
		n.State = models.TaskStateFailed
		n.Type = models.NodeTypeCompound
		return nil
	})
	o.resetInterrupted()
	live, _ = o.tree.CloneNode(root)
	if live.State != models.TaskStateFailed {
		t.Fatalf("failed compound must stay failed, state=%s", live.State)
	}
}

func TestOrchestrator_ParkInterrupted(t *testing.T) {
	cfg := baseTestConfig(t)
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	root := o.tree.RootID

	o.parkInterrupted(root)
	live, _ := o.tree.CloneNode(root)
	if live.State != models.TaskStatePending || live.ErrorMsg != "interrupted" {
		t.Fatalf("empty error must get a reason: %+v", live)
	}

	// A live error message is preserved.
	_ = o.tree.UpdateNode(root, func(n *models.TaskNode) error {
		n.ErrorMsg = "the real reason"
		return nil
	})
	o.parkInterrupted(root)
	live, _ = o.tree.CloneNode(root)
	if live.ErrorMsg != "the real reason" {
		t.Fatalf("existing reason must be kept, got %q", live.ErrorMsg)
	}
}

func TestOrchestrator_GettersAndStatus(t *testing.T) {
	cfg := baseTestConfig(t)
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	if o.SessionID() != cfg.SessionID {
		t.Fatalf("SessionID = %q", o.SessionID())
	}
	if o.Tree() != o.tree || o.StorageDir() != cfg.DataDir {
		t.Fatal("accessors must return the live objects")
	}
	if o.LogPath() != filepath.Join(cfg.DataDir, "logs", cfg.SessionID+".log") {
		t.Fatalf("LogPath = %q", o.LogPath())
	}
	if o.EventPath() != filepath.Join(cfg.DataDir, "events", cfg.SessionID+".jsonl") {
		t.Fatalf("EventPath = %q", o.EventPath())
	}
	if o.parallel() != 1 {
		t.Fatalf("default parallel = %d", o.parallel())
	}
	o.cfg.Parallel = 4
	if o.parallel() != 4 {
		t.Fatalf("explicit parallel lost: %d", o.parallel())
	}
	o.Status() // must print without error
}

func TestOrchestrator_UsageHelpers(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Model = "gpt-4o-mini" // priced
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	u := llm.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000}
	s := o.usageText(u)
	if !strings.Contains(s, "prompt 1000000 + completion 1000000 = 2000000 tokens") ||
		!strings.Contains(s, "est. cost") {
		t.Fatalf("priced usage text = %q", s)
	}
	if kind := o.formatKindUsage(u); !strings.Contains(kind, "est. ") {
		t.Fatalf("kind usage text = %q", kind)
	}

	// An unpriced model omits the cost estimate.
	cfg2 := baseTestConfig(t)
	cfg2.Model = "totally-unknown-model"
	o2, _ := NewFromGoal(cfg2, &llm.ScriptedClient{}, SilentLogger())
	defer o2.Close()
	if s := o2.usageText(u); strings.Contains(s, "est.") {
		t.Fatalf("unpriced model must omit cost: %q", s)
	}
}

func TestOrchestrator_ChatRecordsUsageAndTripsBudget(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.BudgetTokens = 20 // the second chat crosses this ceiling
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return &llm.Response{
			Content: "ok",
			Usage:   llm.Usage{PromptTokens: 4, CompletionTokens: 6},
		}, nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.startBudget(context.Background())

	resp, err := o.chat(context.Background(), "test", o.tree.RootID, llm.Request{})
	if err != nil {
		t.Fatalf("first chat under budget must succeed: %v", err)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage not backfilled: %+v", resp.Usage)
	}
	live, _ := o.tree.CloneNode(o.tree.RootID)
	if live.TokenUsage.Calls != 1 || live.TokenUsage.TotalTokens != 10 {
		t.Fatalf("node usage not recorded: %+v", live.TokenUsage)
	}

	// The second call crosses the ceiling: the response still comes back but
	// the error is context.Canceled.
	_, err = o.chat(context.Background(), "test", o.tree.RootID, llm.Request{})
	if err == nil || err != context.Canceled {
		t.Fatalf("over-budget chat must return context.Canceled, got %v", err)
	}
}

func TestOrchestrator_PublishLeafMergesAndToleratesCommitError(t *testing.T) {
	cfg := baseTestConfig(t) // workdir is NOT a git repository
	o, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	// Enable commits after construction: New() would reject a non-repo
	// workdir, but publishLeaf must tolerate a failed commit at this layer.
	o.cfg.GitCommit = true

	mirror, sb, err := o.newLeafMirror()
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	if err := sb.WriteFile("merged.txt", "from the leaf\n"); err != nil {
		t.Fatal(err)
	}

	leaf, _ := o.tree.CloneNode(o.tree.RootID)
	summary, err := o.publishLeaf(mirror, leaf, "leaf work")
	if err != nil {
		t.Fatalf("publish must tolerate the commit error: %v", err)
	}
	if !strings.Contains(summary, "Merged files: merged.txt") {
		t.Fatalf("merge not reflected in summary: %q", summary)
	}
	data, rerr := os.ReadFile(filepath.Join(cfg.WorkDir, "merged.txt"))
	if rerr != nil || string(data) != "from the leaf\n" {
		t.Fatalf("merged file missing in workspace: %v", rerr)
	}
}

func TestOrchestrator_ReplanAndBuildPlan(t *testing.T) {
	cfg := baseTestConfig(t)
	o, err := NewFromGoal(cfg, scriptedAtomicClient(), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	if err := o.buildPlan(context.Background()); err != nil {
		t.Fatalf("initial plan failed: %v", err)
	}
	if err := o.replan(context.Background(), "focus on tests first"); err != nil {
		t.Fatalf("replan failed: %v", err)
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if !strings.Contains(root.Description, "[User plan adjustment") {
		t.Fatalf("root description missing the adjustment: %q", root.Description)
	}

	// A canceled context stops planning and reports the cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.buildPlan(ctx); err == nil {
		t.Fatal("canceled buildPlan must fail")
	}
}

// scriptedAtomicClient answers every decompose with an atomic plan.
func scriptedAtomicClient() llm.Client {
	return &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, architectMarker) {
			return &llm.Response{Content: `{
  "is_atomic": true,
  "reason": "tiny",
  "contract": {"inputs": [], "outputs": ["out.txt"], "dependencies": [], "constraints": []},
  "dod": {"description": "out.txt exists", "commands": ["test -f out.txt"], "timeout_sec": 30},
  "subtasks": []
}`}, nil
		}
		return &llm.Response{Content: `{"action":"finish","args":{"summary":"done"}}`}, nil
	}}
}

func TestOrchestrator_DecomposeRetriesExhaustedForcesLeaf(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.DecomposeTries = 1 // no retries: the first bad plan falls back
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return &llm.Response{Content: "{not json"}, nil
	}}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	root, _ := o.tree.CloneNode(o.tree.RootID)
	// A retry budget of 1 still degrades to an executable leaf instead of
	// failing the run.
	if err := o.decompose(context.Background(), root); err != nil {
		t.Fatalf("exhausted retries must force a leaf, got %v", err)
	}
	live, _ := o.tree.CloneNode(o.tree.RootID)
	if live.Type != models.NodeTypeLeaf {
		t.Fatalf("forced node must be a leaf, got %s", live.Type)
	}
}

func TestOrchestrator_RunCanceledAtStart(t *testing.T) {
	cfg := baseTestConfig(t)
	o, err := NewFromGoal(cfg, scriptedAtomicClient(), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Run(ctx); err == nil || err != context.Canceled {
		t.Fatalf("canceled Run must return the context error, got %v", err)
	}
}

func TestOrchestrator_WaitBackoffHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBackoff(ctx, 10*time.Millisecond); err == nil {
		t.Fatal("canceled backoff must fail")
	}
	if err := waitBackoff(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("short backoff must succeed: %v", err)
	}
}

func TestFirstLine_Agent(t *testing.T) {
	if got := firstLine("one\ntwo\n"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("  spaced  "); got != "spaced" {
		t.Fatalf("firstLine trim = %q", got)
	}
}
