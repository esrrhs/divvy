package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
	"github.com/esrrhs/go_llm_engine/pkg/models"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
)

// chanGuided is a scripted GuidedIO driven by queued lines.
type chanGuided struct {
	ch      chan string
	printed strings.Builder
}

func newChanGuided(lines ...string) *chanGuided {
	g := &chanGuided{ch: make(chan string, len(lines)+2)}
	for _, l := range lines {
		g.ch <- l
	}
	return g
}
func (g *chanGuided) Lines() <-chan string { return g.ch }
func (g *chanGuided) Printf(format string, a ...any) {
	// Record only plain text so assertions stay simple.
	g.printed.WriteString(format)
}
func (g *chanGuided) close() { close(g.ch) }

func newGuiderWithStub(t *testing.T, responses ...string) (*Guider, *scriptedClient) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.Goal = "test goal"
	cfg.MaxDepth = 3
	cfg.MaxSubtasks = 6
	client := &scriptedClient{responses: responses}
	o, err := NewFromGoal(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	gio := newChanGuided()
	return NewGuider(o, gio), client
}

func TestCommandPredicates(t *testing.T) {
	for _, s := range []string{"/approve", "approve", "go", "yes", "开始", "执行"} {
		if !isApprove(s) {
			t.Errorf("isApprove(%q) = false", s)
		}
	}
	for _, s := range []string{"/abort", "cancel", "取消"} {
		if !isAbort(s) {
			t.Errorf("isAbort(%q) = false", s)
		}
	}
	for _, s := range []string{"/pause", "stop", "暂停"} {
		if !isPause(s) {
			t.Errorf("isPause(%q) = false", s)
		}
	}
	if isPlanEdit("/pause") || !isPlanEdit("/add x") || !isPlanEdit("/redo 1.1") {
		t.Error("isPlanEdit mismatch")
	}
}

func TestReview_Approve(t *testing.T) {
	g, _ := newGuiderWithStub(t)
	// Pre-populate a planned child so the tree looks planned.
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "A", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	g.io.(*chanGuided).ch <- "/approve"

	if err := g.review(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReview_Abort(t *testing.T) {
	g, _ := newGuiderWithStub(t)
	g.io.(*chanGuided).ch <- "/abort"

	err := g.review(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort error, got %v", err)
	}
}

func TestReview_AdjustThenApprove(t *testing.T) {
	// The stub LLM is used once: replan → decompose root returns two leaves.
	planJSON := `{"is_atomic":false,"reasoning":"split",
"subtasks":[
 {"id":"1.1","title":"A","type":"leaf","contract":{},"dod":{}},
 {"id":"1.2","title":"B","type":"leaf","contract":{},"dod":{}}
]}`
	g, client := newGuiderWithStub(t, planJSON)
	cg := g.io.(*chanGuided)
	cg.ch <- "please split into two parts"
	cg.ch <- "/approve"

	if err := g.review(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("expected one re-plan LLM call, got %d", client.calls)
	}
	kids, err := g.o.tree.GetChildren(g.o.tree.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("adjusted plan should have 2 children, got %d", len(kids))
	}
}

func TestHandleExecLine_AddRedoPause(t *testing.T) {
	g, _ := newGuiderWithStub(t)
	ctx := context.Background()

	// /add creates a new pending leaf under the root and moves root pending.
	if err := g.handleExecLine(ctx, "/add write the README"); err != nil {
		t.Fatal(err)
	}
	leaves := g.o.tree.Leaves()
	if len(leaves) != 1 {
		t.Fatalf("expected 1 leaf, got %d", len(leaves))
	}
	id := leaves[0].ID

	// /redo resets an existing node to pending.
	_ = g.o.sched.UpdateNodeState(id, models.TaskStateCompleted, "")
	if err := g.handleExecLine(ctx, "/redo "+id); err != nil {
		t.Fatal(err)
	}
	live, _ := g.o.tree.CloneNode(id)
	if live.State != models.TaskStatePending {
		t.Fatalf("/redo should reset to pending, got %s", live.State)
	}

	// Unknown slash command errors.
	if err := g.handleExecLine(ctx, "/nope"); err == nil {
		t.Fatal("expected error for unknown command")
	}

	// /add with empty instruction errors.
	if err := g.handleExecLine(ctx, "/add   "); err == nil {
		t.Fatal("expected error for empty /add")
	}

	// /pause returns ErrPaused after saving.
	if err := g.handleExecLine(ctx, "/pause"); err != ErrPaused {
		t.Fatalf("expected ErrPaused, got %v", err)
	}
}

func TestAskHookWiring(t *testing.T) {
	g, _ := newGuiderWithStub(t)

	// Install the same closure Run uses, then exercise the handshake from a
	// simulated leaf goroutine.
	g.o.askHook = func(question string) string {
		g.askCh <- question
		return <-g.answerCh
	}

	got := make(chan string, 1)
	go func() {
		got <- g.o.askHook("which port should I use?")
	}()

	q := <-g.askCh
	if q != "which port should I use?" {
		t.Fatalf("unexpected question %q", q)
	}
	g.answerCh <- "use 8080"

	if ans := <-got; ans != "use 8080" {
		t.Fatalf("unexpected answer %q", ans)
	}
}

func TestWorkerAskBatchFallback(t *testing.T) {
	// In batch mode (no askHook) an "ask" must be answered with guidance to
	// proceed, and the worker then finishes without blocking.
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Goal = "test goal"
	cfg.MaxSteps = 6
	client := &scriptedClient{responses: []string{
		`{"thought":"need input","action":"ask","args":{"question":"port?"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"finished it"}}`,
	}}
	o, err := NewFromGoal(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	root := o.tree.GetRoot()
	sb, err := tools.NewSandbox(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := o.runWorker(context.Background(), sb, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "finished it" {
		t.Fatalf("unexpected summary %q", summary)
	}
	if client.calls != 2 {
		t.Fatalf("ask fallback should feed one answer then finish, got %d calls", client.calls)
	}
}

// blockingClient blocks in Chat until its context is canceled; used to hold a
// leaf in the RUNNING state so the controller can interject.
type blockingClient struct {
	started chan struct{}
}

func (c *blockingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// installAskHook wires the same askHook Run uses, and returns a channel that
// closes once the controller has received the worker's question (i.e. it is
// now waiting for the user's answer).
func installAskHook(g *Guider) chan struct{} {
	delivered := make(chan struct{})
	g.o.askHook = func(question string) string {
		g.askCh <- question
		close(delivered)
		return <-g.answerCh
	}
	return delivered
}

func TestRunLeafInteractive_PauseWhileLeafRuns(t *testing.T) {
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "long leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")

	bc := &blockingClient{started: make(chan struct{}, 1)}
	g.o.llm = bc

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-bc.started // the leaf is now blocked in the model call
	g.io.(*chanGuided).ch <- "/pause"

	select {
	case err := <-done:
		if err != ErrPaused {
			t.Fatalf("expected ErrPaused, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLeafInteractive did not return after /pause")
	}

	// The interrupted leaf must be runnable again on resume.
	n, _ := g.o.tree.CloneNode("1.1")
	if n.State != models.TaskStatePending {
		t.Fatalf("paused leaf should be pending again, got %s", n.State)
	}
}

func TestRunLeafInteractive_PauseWhileAsking(t *testing.T) {
	// Regression: /pause while the worker is blocked on an answer used to
	// deadlock the controller on <-doneCh; unblockAsker must wake the worker.
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "asking leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")

	client := &scriptedClient{responses: []string{
		`{"thought":"need input","action":"ask","args":{"question":"which port?"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"finished"}}`,
	}}
	g.o.llm = client
	askDelivered := installAskHook(g)

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-askDelivered // controller is now waiting for the user's answer
	g.io.(*chanGuided).ch <- "/pause"

	select {
	case err := <-done:
		if err != ErrPaused {
			t.Fatalf("expected ErrPaused, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLeafInteractive deadlocked on /pause during ask")
	}

	// The leaf was interrupted and put back to pending.
	n, _ := g.o.tree.CloneNode("1.1")
	if n.State != models.TaskStatePending {
		t.Fatalf("paused leaf should be pending again, got %s", n.State)
	}
}

func TestRunLeafInteractive_AskAnswerThenFinish(t *testing.T) {
	// Full handshake: worker asks, the user answers, the worker continues and
	// the leaf completes.
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "asking leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")

	client := &scriptedClient{responses: []string{
		`{"thought":"need input","action":"ask","args":{"question":"which port?"}}`,
		`{"thought":"done","action":"finish","args":{"summary":"used 8080"}}`,
	}}
	g.o.llm = client
	askDelivered := installAskHook(g)

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-askDelivered
	g.io.(*chanGuided).ch <- "8080"

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("leaf should complete, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("leaf did not finish after the question was answered")
	}

	found := false
	for _, m := range client.lastMsgs {
		if strings.Contains(m.Content, "8080") {
			found = true
		}
	}
	if !found {
		t.Fatal("worker never received the user's answer")
	}

	n, _ := g.o.tree.CloneNode("1.1")
	if n.State != models.TaskStateCompleted {
		t.Fatalf("leaf should be completed, got %s", n.State)
	}
	if n.ResultSummary != "used 8080" {
		t.Fatalf("unexpected summary %q", n.ResultSummary)
	}
}

func TestGuidedRun_SkipsReviewOnResume(t *testing.T) {
	// A resumed session that already executed some leaves must skip planning
	// and review and continue straight into execution — no /approve needed.
	ws := t.TempDir()
	dataDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.WorkDir = ws
	cfg.DataDir = dataDir
	cfg.Goal = "test goal"
	cfg.SessionID = "sess_resume_guided"

	// Build a saved session: two leaves, one already completed.
	o0, err := NewFromGoal(cfg, &scriptedClient{responses: []string{`{"action":"finish","args":{"summary":"x"}}`}}, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o0.tree.AddChild(o0.tree.RootID, "1.1", "done leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := o0.tree.AddChild(o0.tree.RootID, "1.2", "pending leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o0.sched.UpdateNodeState("1.1", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := o0.checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Resume: the pending leaf's worker finishes in one call.
	client := &scriptedClient{responses: []string{
		`{"thought":"done","action":"finish","args":{"summary":"1.2 done"}}`,
	}}
	o, err := Load(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}

	// No input lines at all: if review were not skipped this would block on
	// /approve forever and the timeout would fire.
	gio := newChanGuided()
	g := NewGuider(o, gio)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := g.Run(ctx); err != nil {
		t.Fatal(err)
	}

	n, _ := o.tree.CloneNode("1.2")
	if n.State != models.TaskStateCompleted {
		t.Fatalf("resumed leaf should complete without review, got %s", n.State)
	}
}

func TestReplan_FailureKeepsTree(t *testing.T) {
	// When the re-plan aborts (here: canceled context), the old children are
	// discarded, the error propagates, and the feedback is recorded in the
	// root description so the next attempt still sees it.
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "A", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := g.o.replan(ctx, "split differently")
	if err == nil {
		t.Fatal("expected replan to fail on canceled context")
	}

	// The tree must still contain only the root; no dangling children.
	kids, kerr := g.o.tree.GetChildren(g.o.tree.RootID)
	if kerr != nil {
		t.Fatal(kerr)
	}
	if len(kids) != 0 {
		t.Fatalf("children should be reset before re-planning, got %d", len(kids))
	}
	if !strings.Contains(g.o.tree.GetRoot().Description, "split differently") {
		t.Fatal("user feedback should be recorded in the root description")
	}
}

func TestReview_EOFDoesNotApprove(t *testing.T) {
	// Regression: closing stdin during review used to return nil, which Run
	// treated as approval and started execution unattended.
	g, _ := newGuiderWithStub(t)
	g.io.(*chanGuided).close()

	err := g.review(context.Background())
	if err != errStdinClosed {
		t.Fatalf("expected errStdinClosed, got %v", err)
	}
	// Nothing was executed: no children exist.
	kids, err := g.o.tree.GetChildren(g.o.tree.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 0 {
		t.Fatalf("plan must not execute on EOF, found %d children", len(kids))
	}
}

func TestGuidedRun_EOFBeforeApprovalSavesPlan(t *testing.T) {
	// End-to-end: a planned tree, stdin closes before /approve → Run exits
	// cleanly and every leaf stays pending.
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "A", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.2", "B", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	g.io.(*chanGuided).close()

	if err := g.Run(context.Background()); err != nil {
		t.Fatalf("EOF should be a clean exit, got %v", err)
	}
	for _, id := range []string{"1.1", "1.2"} {
		n, _ := g.o.tree.CloneNode(id)
		if n.State != models.TaskStatePending {
			t.Fatalf("%s must stay pending, got %s", id, n.State)
		}
	}
}

func TestRunLeafInteractive_EOFInterruptsLeaf(t *testing.T) {
	g, _ := newGuiderWithStub(t)
	if _, err := g.o.tree.AddChild(g.o.tree.RootID, "1.1", "long leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	leaf, _ := g.o.tree.CloneNode("1.1")

	bc := &blockingClient{started: make(chan struct{}, 1)}
	g.o.llm = bc

	done := make(chan error, 1)
	go func() { done <- g.runLeafInteractive(context.Background(), leaf) }()

	<-bc.started
	g.io.(*chanGuided).close()

	select {
	case err := <-done:
		if err != errStdinClosed {
			t.Fatalf("expected errStdinClosed, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLeafInteractive did not return after EOF")
	}

	n, _ := g.o.tree.CloneNode("1.1")
	if n.State != models.TaskStatePending {
		t.Fatalf("interrupted leaf should be pending again, got %s", n.State)
	}
}

func TestGuidedRun_BudgetTokensCancels(t *testing.T) {
	// Regression: -budget-tokens silently did nothing in guided runs. A
	// resumed session (one leaf done, one pending) with a 5-token ceiling must
	// stop as soon as the pending leaf's first model call returns 15 tokens.
	ws := t.TempDir()
	dataDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.WorkDir = ws
	cfg.DataDir = dataDir
	cfg.Goal = "test goal"
	cfg.SessionID = "sess_guided_budget"
	cfg.BudgetTokens = 5

	o0, err := NewFromGoal(cfg, &scriptedClient{responses: []string{`{"action":"finish","args":{"summary":"x"}}`}}, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o0.tree.AddChild(o0.tree.RootID, "1.1", "done leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if _, err := o0.tree.AddChild(o0.tree.RootID, "1.2", "pending leaf", "desc", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := o0.sched.UpdateNodeState("1.1", models.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := o0.checkpoint(); err != nil {
		t.Fatal(err)
	}

	client := &scriptedClient{responses: []string{
		`{"thought":"done","action":"finish","args":{"summary":"1.2 done"}}`,
	}}
	o, err := Load(cfg, client, NewLogger(false))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGuider(o, newChanGuided())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = g.Run(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled when budget trips, got %v", err)
	}

	n, _ := o.tree.CloneNode("1.2")
	if n.State != models.TaskStatePending {
		t.Fatalf("budget-canceled leaf should be pending again, got %s", n.State)
	}
	if client.calls < 1 {
		t.Fatal("pending leaf should have been attempted")
	}
}
