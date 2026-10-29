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

// TestPrepareSoloRoot_OperatorAcceptanceWins: an explicit -dod is a contract.
// Derived acceptance is a guess, and a guess must never override the operator,
// not even when the guess looks more specific (go test ./... vs a shell check).
func TestPrepareSoloRoot_OperatorAcceptanceWins(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, nil)
	o.cfg.DoDCommands = []string{"go vet ./...", "go test ./..."}
	// Make the workspace look like a Go project so the derived acceptance
	// would have something tempting to say.
	if err := os.WriteFile(filepath.Join(o.sandbox.Root, "go.mod"), []byte("module app\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	auto, err := o.prepareSoloRoot()
	if err != nil {
		t.Fatal(err)
	}
	if auto {
		t.Fatal("operator acceptance must not be re-resolved as the workspace changes")
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if got := strings.Join(root.DoD.Commands, " ; "); got != "go vet ./... ; go test ./..." {
		t.Fatalf("acceptance = %q, want the operator's two commands", got)
	}
	if root.DoD.TimeoutSec != soloAcceptanceTimeout {
		t.Errorf("timeout = %d, want %d", root.DoD.TimeoutSec, soloAcceptanceTimeout)
	}
	// And it must survive an auto-refresh attempt.
	s := &soloSession{o: o, sb: o.sandbox, autoAcceptance: auto}
	s.refreshAcceptance(root)
	if len(root.DoD.Commands) != 2 {
		t.Fatalf("acceptance was overwritten: %v", root.DoD.Commands)
	}
}

// TestRunSolo_EscalatesOnRepeatedFailure: the second identical failure must
// change the instruction, not just repeat it. A measured phys2d run failed the
// same penetration check twice and got worse (0.0985 -> 0.3973) because both
// repairs were guesses at the same lines.
func TestRunSolo_EscalatesOnRepeatedFailure(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"trust me"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f never-created.txt"})
	o.cfg.SoloRounds = 3
	o.cfg.MaxStall = 10 // do not stop early; we want to inspect round 3's prompt

	if err := o.RunSolo(context.Background()); err == nil {
		t.Fatal("RunSolo should have failed")
	}
	if len(client.Requests) < 3 {
		t.Fatalf("only %d requests, want 3 rounds", len(client.Requests))
	}

	contents := func(i int) string {
		var b strings.Builder
		for _, m := range client.Requests[i].Messages {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
		return b.String()
	}
	if !strings.Contains(contents(1), "Acceptance FAILED") {
		t.Error("round 2 should carry the plain repair prompt")
	}
	if strings.Contains(contents(1), "Do NOT edit the same lines again") {
		t.Error("round 2 escalated before the same failure had repeated")
	}
	if got := contents(2); !strings.Contains(got, "Do NOT edit the same lines again") {
		t.Errorf("round 3 did not escalate to a strategy change")
	} else if !strings.Contains(got, "Observe it") {
		t.Error("escalation does not demand an observation")
	}
}

// TestRunSolo_StopsEarlyOnStall: repeating the same failure is not progress,
// so -max-stall applies to solo too. Without this the knob is documented for
// leaves and silently ignored by the mode that runs everything in one loop.
func TestRunSolo_StopsEarlyOnStall(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"trust me"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f never-created.txt"})
	o.cfg.SoloRounds = 10
	o.cfg.MaxStall = 2

	err := o.RunSolo(context.Background())
	if err == nil {
		t.Fatal("RunSolo should have failed")
	}
	if !strings.Contains(err.Error(), "repeated 2 times") {
		t.Errorf("error = %q, want the stall reason", err)
	}
	if len(client.Requests) != 2 {
		t.Fatalf("%d requests, want 2 — the stall guard should stop the loop", len(client.Requests))
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if root.State != models.TaskStateFailed {
		t.Fatalf("root state = %s, want FAILED", root.State)
	}
}

// TestSoloNoteFailure_ResetsOnDifferentFailure: only an identical failure
// counts as a stall. A run whose acceptance improves should keep its budget.
func TestSoloNoteFailure_ResetsOnDifferentFailure(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, []string{"test -f a.txt"})
	s := &soloSession{o: o}
	if got := s.noteFailure(VerifyResult{Command: "go test ./...", Output: "ball penetrated ground: y=-0.0985"}); got != 1 {
		t.Fatalf("first failure run = %d", got)
	}
	// Same failure mode, different number: must still count as a repeat.
	if got := s.noteFailure(VerifyResult{Command: "go test ./...", Output: "ball penetrated ground: y=-0.3973"}); got != 2 {
		t.Fatalf("repeated failure run = %d, want 2", got)
	}
	// A genuinely different failure is new information, not a stall.
	if got := s.noteFailure(VerifyResult{Command: "go test ./...", Output: "./vec: undefined: Dot"}); got != 1 {
		t.Fatalf("different failure run = %d, want 1", got)
	}
	// A changed acceptance means the fingerprints are not comparable.
	if got := s.noteFailure(VerifyResult{Command: "go vet ./...", Output: "ball penetrated ground: y=-0.0985"}); got != 1 {
		t.Fatalf("failure under a new command run = %d, want 1", got)
	}
}

func joinedContents(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// TestRunSolo_ResumesTranscript: the transcript is solo's state. Without it a
// resumed run starts over against a workspace it has no memory of building —
// which a measured run answered by rewriting every file from scratch.
func TestRunSolo_ResumesTranscript(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		return action(t, `{"thought":"write","action":"write_file","args":{"path":"hello.txt","content":"hi\n"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f hello.txt"})
	path := o.storage.GetSoloHistoryPath(o.tree.ID)
	if err := saveSoloHistory(path, []llm.Message{
		{Role: llm.RoleSystem, Content: "STALE-SYSTEM-PROMPT"},
		{Role: llm.RoleUser, Content: "EARLIER-GOAL-TURN"},
		{Role: llm.RoleAssistant, Content: "I already built vec2 and shapes"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := o.RunSolo(context.Background()); err != nil {
		t.Fatalf("RunSolo: %v", err)
	}
	if len(client.Requests) == 0 {
		t.Fatal("no requests recorded")
	}
	first := joinedContents(client.Requests[0].Messages)
	if !strings.Contains(first, "EARLIER-GOAL-TURN") {
		t.Error("resume dropped the earlier conversation")
	}
	if !strings.Contains(first, "I already built vec2 and shapes") {
		t.Error("resume dropped the earlier assistant turn")
	}
	if !strings.Contains(first, "resuming") {
		t.Error("resume did not tell the model it is continuing")
	}
	if strings.Contains(first, "STALE-SYSTEM-PROMPT") {
		t.Error("resume kept the stale system prompt instead of refreshing it")
	}
}

// TestSaveLoadSoloHistory_ToleratesTornTail: a crash mid-write must cost the
// last message, not the whole transcript.
func TestSaveLoadSoloHistory_ToleratesTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleAssistant, Content: "call", ToolCalls: []llm.ToolCall{
			{ID: "t1", Function: llm.ToolFunction{Name: "read_file", Arguments: `{"path":"a.go"}`}},
		}},
		{Role: llm.RoleTool, ToolCallID: "t1", Name: "read_file", Content: "package a"},
	}
	if err := saveSoloHistory(path, msgs); err != nil {
		t.Fatal(err)
	}
	got, err := loadSoloHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].ToolCalls[1-1].Function.Name != "read_file" || got[2].ToolCallID != "t1" {
		t.Fatalf("round trip lost tool calls: %+v", got)
	}

	// Simulate a torn write: valid prefix, half a JSON object at the end.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"role":"assistant","content":"hal`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err = loadSoloHistory(path)
	if err != nil {
		t.Fatalf("torn tail should not fail the load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d messages, want the 3 complete ones", len(got))
	}
	// Unknown path is a first run, not an error.
	if m, err := loadSoloHistory(filepath.Join(t.TempDir(), "missing.jsonl")); err != nil || m != nil {
		t.Fatalf("missing transcript = %v, %v", m, err)
	}
}

func TestPrepareSoloRoot_RefusesCompletedSession(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, []string{"test -f hello.txt"})
	if _, err := o.prepareSoloRoot(); err != nil {
		t.Fatal(err)
	}
	if err := o.tree.UpdateNode(o.tree.RootID, func(n *models.TaskNode) error {
		n.State = models.TaskStateCompleted
		n.IntegrationVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.prepareSoloRoot(); err != errSoloAlreadyComplete {
		t.Fatalf("err = %v, want errSoloAlreadyComplete", err)
	}
	// -resume on a finished solo session is a no-op, not a failure.
	if err := o.RunSolo(context.Background()); err != nil {
		t.Fatalf("RunSolo on a completed session = %v, want nil", err)
	}
}

// TestSoloTaskPrompt_WarnsAboutExistingWork pins the wording that stops a
// fresh run from rewriting a workspace it was handed.
func TestSoloTaskPrompt_WarnsAboutExistingWork(t *testing.T) {
	o := newSoloOrch(t, &llm.ScriptedClient{}, []string{"test -f hello.txt"})
	root, _ := o.tree.CloneNode(o.tree.RootID)

	if got := o.soloTaskPrompt(o.sandbox, root); strings.Contains(got, "NOT empty") {
		t.Error("empty workspace must not be described as existing work")
	}
	if err := os.WriteFile(filepath.Join(o.sandbox.Root, "existing.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := o.soloTaskPrompt(o.sandbox, root)
	if !strings.Contains(got, "NOT empty") {
		t.Error("non-empty workspace is not flagged")
	}
	if !strings.Contains(got, "Read a file before you replace it") {
		t.Error("prompt does not forbid blind overwrites")
	}
	if !strings.Contains(got, "existing.go") {
		t.Error("prompt does not list the existing files")
	}
}

// TestRunSolo_InterruptsStepThrash: round-level stall detection only fires at a
// round boundary, but a 60-step round is where the guessing actually happens.
// A measured run spent 38 steps alternating edit/test on one assertion without
// converging; the loop has to be interrupted where it happens.
func TestRunSolo_InterruptsStepThrash(t *testing.T) {
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		// The same command, failing identically, forever.
		return action(t, `{"thought":"try again","action":"run_bash","args":{"command":"test -f never.txt"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f never-created.txt"})
	o.cfg.SoloSteps = 6
	o.cfg.SoloRounds = 1

	if err := o.RunSolo(context.Background()); err == nil {
		t.Fatal("RunSolo should have failed")
	}
	if len(client.Requests) < 3 {
		t.Fatalf("only %d requests", len(client.Requests))
	}
	// The intervention lands in the conversation and stays there, so the last
	// request is where to count how many were issued.
	last := joinedContents(client.Requests[len(client.Requests)-1].Messages)
	nags := strings.Count(last, "Stop.")
	if nags == 0 {
		t.Fatal("the guess loop was never interrupted")
	}
	if nags > soloStepNags {
		t.Fatalf("interrupted %d times in one round, want at most %d", nags, soloStepNags)
	}
	if !strings.Contains(last, "same output as your previous attempt") {
		t.Error("intervention does not explain why it fired")
	}
	if !strings.Contains(last, "scratch/main.go") {
		t.Error("intervention does not say where a scratch program must live")
	}
	if !strings.Contains(last, "Do not weaken or delete the check") {
		t.Error("intervention does not forbid weakening the assertion")
	}
}

// TestRunSolo_AuditsGoalBeforeFinishing: acceptance commands are a floor. A
// measured run passed all of them while leaving prose requirements
// unimplemented, so one extra turn is spent checking the goal against the disk.
func TestRunSolo_AuditsGoalBeforeFinishing(t *testing.T) {
	calls := 0
	client := &llm.ScriptedClient{Handle: func(context.Context, llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return action(t, `{"thought":"write","action":"write_file","args":{"path":"hello.txt","content":"hi\n"}}`), nil
		}
		return action(t, `{"thought":"done","action":"finish","args":{"summary":"wrote hello.txt"}}`), nil
	}}
	o := newSoloOrch(t, client, []string{"test -f hello.txt"})
	// The audit must not be starved by the round budget: it gets its own.
	o.cfg.SoloRounds = 1

	if err := o.RunSolo(context.Background()); err != nil {
		t.Fatalf("RunSolo: %v", err)
	}
	root, _ := o.tree.CloneNode(o.tree.RootID)
	if root.State != models.TaskStateCompleted {
		t.Fatalf("root state = %s, want COMPLETED", root.State)
	}
	if len(client.Requests) != 3 {
		t.Fatalf("%d requests, want write + finish + audit turn", len(client.Requests))
	}
	audit := joinedContents(client.Requests[2].Messages)
	for _, want := range []string{"audit the goal itself", "table-driven", "print frequency", o.tree.Goal} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit prompt is missing %q", want)
		}
	}
	// A single audit, not one per round.
	if n := strings.Count(audit, "audit the goal itself"); n != 1 {
		t.Fatalf("audit appears %d times", n)
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
