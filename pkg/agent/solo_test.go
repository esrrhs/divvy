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

func action(t *testing.T, s string) *llm.Response {
	t.Helper()
	return &llm.Response{Content: s}
}

func newSoloOrch(t *testing.T, client llm.Client, acceptance []string) *Orchestrator {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_solo"
	cfg.Goal = "create hello.txt containing hi"
	cfg.Stream = false
	cfg.SoloSteps = 6
	cfg.SoloRounds = 2
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if acceptance != nil {
		if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
			n.DoD = models.DoD{Commands: acceptance, TimeoutSec: 30}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

// TestRunSolo_RepairsFailureInSameConversation is the behavioural difference
// from tree mode: the acceptance failure goes back into the conversation that
// produced the code. A leaf would instead be reset into a fresh context and
// re-derive the project from its contract.
func TestRunSolo_RepairsFailureInSameConversation(t *testing.T) {
	calls := 0
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		calls++
		switch calls {
		case 1:
			// Claims done without writing anything: acceptance must catch it.
			return action(t, `{"thought":"done","action":"finish","args":{"summary":"wrote it"}}`), nil
		case 2:
			return action(t, `{"thought":"write","action":"write_file","args":{"path":"hello.txt","content":"hi\n"}}`), nil
		default:
			return action(t, `{"thought":"done","action":"finish","args":{"summary":"wrote hello.txt"}}`), nil
		}
	}}
	o := newSoloOrch(t, client, []string{"test -f hello.txt"})

	if err := o.RunSolo(context.Background()); err != nil {
		t.Fatalf("RunSolo: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(o.sandbox.Root, "hello.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("hello.txt = %q, %v", b, err)
	}

	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok || root.State != models.TaskStateCompleted {
		t.Fatalf("root state = %+v (ok=%v)", root, ok)
	}
	if !root.IntegrationVerified {
		t.Error("acceptance passed but IntegrationVerified is false")
	}

	// Round 2's request must carry both the original goal and the failure.
	if len(client.Requests) < 2 {
		t.Fatalf("only %d requests", len(client.Requests))
	}
	second := client.Requests[1]
	var joined strings.Builder
	for _, m := range second.Messages {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	got := joined.String()
	if !strings.Contains(got, "Acceptance FAILED") {
		t.Error("repair round does not carry the acceptance failure")
	}
	if !strings.Contains(got, "test -f hello.txt") {
		t.Error("repair round does not name the failing command")
	}
	if !strings.Contains(got, o.tree.Goal) {
		t.Error("repair round lost the original goal")
	}
	// Same conversation, so the first round's own claim is still visible.
	if !strings.Contains(got, "wrote it") {
		t.Error("repair round dropped the earlier assistant turn")
	}
}

func TestRunSolo_FailsAfterRoundsExhausted(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"trust me"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f never-created.txt"})

	if err := o.RunSolo(context.Background()); err == nil {
		t.Fatal("RunSolo accepted a goal whose acceptance never passed")
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if root.State != models.TaskStateFailed {
		t.Fatalf("root state = %s, want FAILED", root.State)
	}
	if !strings.Contains(root.ErrorMsg, "rounds") {
		t.Errorf("error message = %q", root.ErrorMsg)
	}
}

func TestPrepareSoloRoot_RejectsPlannedTree(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"is_atomic":true,"subtasks":[]}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f hello.txt"})
	if _, err := o.tree.AddChild(o.tree.RootID, "child", "child", "d", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := o.prepareSoloRoot(); err == nil {
		t.Fatal("prepareSoloRoot accepted a tree that already has subtasks")
	}
}

func TestSoloCompose_ElidesMiddleIntoDigest(t *testing.T) {
	s := &soloSession{budget: 2000, msgs: []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "goal"},
	}}
	for i := 0; i < 200; i++ {
		s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("x", 200)})
	}
	s.journal.addFile("vec/vec.go")
	s.journal.noteFailure("go build ./...")

	out := s.compose(1, 10)
	if out[0].Content != "sys" || out[1].Content != "goal" {
		t.Fatal("system prompt or goal was dropped")
	}
	var digestSeen bool
	total := 0
	for _, m := range out {
		total += len(m.Content)
		if strings.Contains(m.Content, "progress log") {
			digestSeen = true
			if !strings.Contains(m.Content, "vec/vec.go") {
				t.Error("digest does not mention files already written")
			}
			if !strings.Contains(m.Content, "go build ./...") {
				t.Error("digest does not mention earlier failures")
			}
		}
	}
	if !digestSeen {
		t.Fatal("no digest for the elided window")
	}
	if total > 4000 {
		t.Fatalf("window is %d chars, budget was 2000", total)
	}
	// The tail must be the recent messages, not the oldest ones.
	if !strings.HasPrefix(out[len(out)-1].Content, "xxx") {
		t.Fatalf("tail message = %q", out[len(out)-1].Content)
	}
}

func TestSoloCompose_KeepsToolGroupsIntact(t *testing.T) {
	s := &soloSession{budget: 300, msgs: []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "goal"},
	}}
	for i := 0; i < 20; i++ {
		s.msgs = append(s.msgs,
			llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("a", 50),
				ToolCalls: []llm.ToolCall{{ID: "t1", Function: llm.ToolFunction{Name: "read_file", Arguments: "{}"}}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: "t1", Name: "read_file", Content: strings.Repeat("r", 50)},
		)
	}
	out := s.compose(1, 10)
	// Skipping the head and the digest, the first retained message must not be
	// an orphaned tool reply: OpenAI-compatible servers reject that with 400.
	for i := 2; i < len(out); i++ {
		if out[i].Role == llm.RoleTool {
			prev := out[i-1]
			if prev.Role != llm.RoleAssistant || len(prev.ToolCalls) == 0 {
				t.Fatalf("retained window starts with an orphaned tool reply at %d", i)
			}
			break
		}
	}
}

func TestSoloContextBudget(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, nil)
	if got := o.soloContextBudget(); got != soloContextChars {
		t.Fatalf("default budget = %d, want %d", got, soloContextChars)
	}
	o.cfg.SoloContextChars = 12_345
	if got := o.soloContextBudget(); got != 12_345 {
		t.Fatalf("override = %d", got)
	}
}

// TestRunSolo_PersistsFinalState guards a bug the smoke test found: solo has
// no scheduler loop to checkpoint for it, so without an explicit write the
// session file still said PENDING after a successful run and -status/-report
// lied about the outcome.
func TestRunSolo_PersistsFinalState(t *testing.T) {
	calls := 0
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return action(t, `{"thought":"write","action":"write_file","args":{"path":"ok.txt","content":"yes\n"}}`), nil
		}
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"wrote ok.txt"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f ok.txt"})
	if err := o.RunSolo(context.Background()); err != nil {
		t.Fatalf("RunSolo: %v", err)
	}

	reloaded, err := Load(o.cfg, nil, SilentLogger())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	root, ok := reloaded.tree.CloneNode(reloaded.tree.RootID)
	if !ok {
		t.Fatal("root missing after reload")
	}
	if root.State != models.TaskStateCompleted {
		t.Fatalf("persisted root state = %s, want COMPLETED", root.State)
	}
	if !root.IntegrationVerified {
		t.Error("persisted root lost IntegrationVerified")
	}
}

// TestRefreshAcceptance_UpgradesWhenProjectAppears guards a bug found by the
// first real solo run: the workspace was empty when acceptance was resolved,
// so the run verified a generic placeholder and "passing" proved nothing. The
// auto acceptance has to re-resolve once the project exists.
func TestRefreshAcceptance_UpgradesWhenProjectAppears(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, nil)
	auto, err := o.prepareSoloRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !auto {
		t.Fatal("empty DoD should be treated as auto-generated")
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if len(root.DoD.Commands) != 1 || root.DoD.Commands[0] == "ls" {
		t.Fatalf("generic acceptance = %v, want a non-empty-workspace check", root.DoD.Commands)
	}

	s := &soloSession{o: o, sb: o.sandbox, autoAcceptance: true, msgs: []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "goal"},
	}}
	s.refreshAcceptance(root)
	if strings.Contains(strings.Join(root.DoD.Commands, " "), "go test") {
		t.Fatal("upgraded to a Go acceptance before go.mod existed")
	}

	if err := os.WriteFile(filepath.Join(o.sandbox.Root, "go.mod"), []byte("module app\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.refreshAcceptance(root)
	if got := strings.Join(root.DoD.Commands, " && "); got != "go test ./..." {
		t.Fatalf("acceptance = %q, want go test ./...", got)
	}
	// The model must be told, or it will finish against the old contract.
	if len(s.msgs) != 3 {
		t.Fatalf("accepted upgrade without telling the model (%d messages)", len(s.msgs))
	}
	if !strings.Contains(s.msgs[2].Content, "acceptance") {
		t.Errorf("notice = %q", s.msgs[2].Content)
	}
	// The node is updated too, so -status shows the real acceptance.
	persisted, _ := o.tree.CloneNode(o.tree.RootID)
	if strings.Join(persisted.DoD.Commands, " ") != "go test ./..." {
		t.Errorf("tree DoD = %v", persisted.DoD.Commands)
	}
}

// TestRefreshAcceptance_KeepsCallerDoD: an explicit acceptance is a contract.
func TestRefreshAcceptance_KeepsCallerDoD(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, []string{"test -f hello.txt"})
	auto, err := o.prepareSoloRoot()
	if err != nil {
		t.Fatal(err)
	}
	if auto {
		t.Fatal("explicit DoD must not be re-resolved")
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if err := os.WriteFile(filepath.Join(o.sandbox.Root, "go.mod"), []byte("module app\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &soloSession{o: o, sb: o.sandbox, autoAcceptance: false, msgs: []llm.Message{{}, {}}}
	s.refreshAcceptance(root)
	if got := strings.Join(root.DoD.Commands, " "); got != "test -f hello.txt" {
		t.Fatalf("explicit acceptance was overwritten: %q", got)
	}
}

// TestLoadSnapshot_DoesNotReviveFailedSoloRoot pins the difference between the
// two load paths. A failed solo session is a tree made of one leaf, and
// resetInterrupted turns a failed leaf back into PENDING so -resume retries it.
// Read-only views must not do that: -report on such a session used to print
// "state PENDING" directly above its own "outcome failed" line.
func TestLoadSnapshot_DoesNotReviveFailedSoloRoot(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"trust me"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f never.txt"})
	if err := o.RunSolo(context.Background()); err == nil {
		t.Fatal("RunSolo should have failed")
	}

	snap, err := LoadSnapshot(o.cfg, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	root, _ := snap.tree.CloneNode(snap.tree.RootID)
	if root.State != models.TaskStateFailed {
		t.Fatalf("snapshot state = %s, want FAILED as saved", root.State)
	}

	resumed, err := Load(o.cfg, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	rroot, _ := resumed.tree.CloneNode(resumed.tree.RootID)
	if rroot.State != models.TaskStatePending {
		t.Fatalf("resume state = %s, want PENDING so the work is retried", rroot.State)
	}
}
