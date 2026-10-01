package agent

import (
	"context"
	"strings"
	"testing"

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
