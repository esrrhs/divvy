package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
)

const atomicFilePlan = `{
  "is_atomic": true,
  "reason": "one file",
  "contract": {"outputs": ["x.txt"]},
  "dod": {"commands": ["test -f x.txt"]}
}`

// TestWorker_FinishGateRejectsConflictThenAccepts proves the deterministic
// pre-finish gate closes the loop without human help: a finish while the new
// (untracked) file still carries conflict markers is rejected and the report
// is fed back; after the model rewrites the file cleanly, finish is accepted.
func TestWorker_FinishGateRejectsConflictThenAccepts(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	// Gate only exists in a git repository.
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, ".gitkeep"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "baseline")

	const badFile = "<<<<<<< HEAD\nkeep\n=======\n>>>>>>> branch\n"

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: atomicFilePlan}, nil
			}
			rejected, writes := false, 0
			for _, m := range req.Messages {
				if m.Role != llm.RoleUser {
					continue
				}
				if strings.Contains(m.Content, "finish REJECTED") {
					rejected = true
				}
				if strings.Contains(m.Content, "Tool write_file result") {
					writes++
				}
			}
			switch {
			case rejected && writes >= 2:
				return jsonAction("finish", map[string]string{"summary": "fixed and done"}), nil
			case rejected:
				// Rewrite the whole file, removing every conflict marker.
				return jsonAction("write_file", map[string]string{
					"path": "x.txt", "content": "resolved content\n",
				}), nil
			case writes >= 1:
				// Initial happy-path finish attempt — must be rejected.
				return jsonAction("finish", map[string]string{"summary": "done"}), nil
			default:
				return jsonAction("write_file", map[string]string{
					"path": "x.txt", "content": badFile,
				}), nil
			}
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_finish_gate"
	cfg.Goal = "write x.txt"
	cfg.Stream = false
	cfg.MaxRetries = 3

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("task should complete after fixing the gate findings:\n%s", o.tree.RenderVisualTree())
	}

	raw, err := os.ReadFile(filepath.Join(work, "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "<<<<<<<") || strings.Contains(string(raw), ">>>>>>>") {
		t.Fatalf("conflict markers must be gone in the delivered file:\n%s", raw)
	}
	if !strings.Contains(string(raw), "resolved content") {
		t.Fatalf("delivered file should be the clean rewrite, got:\n%s", raw)
	}

	// Sanity: the rejection really happened during the run (at least one
	// write before the rejected finish, then the repair write).
	if writes := countWorkerWriteResults(client); writes < 2 {
		t.Fatalf("expected the repair rewrite after rejection, got %d write calls", writes)
	}
}

// countWorkerWriteResults counts write_file tool results seen across all
// scripted worker requests.
func countWorkerWriteResults(c *llm.ScriptedClient) int {
	n := 0
	for _, req := range c.Requests {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, "Tool write_file result") {
				n++
			}
		}
	}
	return n
}
