package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/llm"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
}

func gitLogSubjects(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "--format=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	return string(out)
}

// TestOrchestrator_GitCommitPerLeaf verifies each leaf's merged changes land
// as one auditable commit, referenced from the leaf's result summary.
func TestOrchestrator_GitCommitPerLeaf(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()
	initGitRepo(t, work)

	client := &llm.ScriptedClient{Handle: twoLeafWorkerHandle(nil)}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_git_commit"
	cfg.Goal = "two independent files"
	cfg.Stream = false
	cfg.Parallel = 2
	cfg.Isolate = true
	cfg.GitCommit = true
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}

	log := gitLogSubjects(t, work)
	for _, id := range []string{"alpha", "beta"} {
		if !strings.Contains(log, "leaf("+id+"):") {
			t.Fatalf("missing commit for %s in:\n%s", id, log)
		}
		n, _ := o.tree.CloneNode(id)
		if !strings.Contains(n.ResultSummary, "Commit: ") {
			t.Fatalf("%s summary missing commit hash: %q", id, n.ResultSummary)
		}
	}
	// Workspace files are tracked by the commits, not left uncommitted.
	out, err := exec.Command("git", "-C", work, "status", "--porcelain").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("repo not clean after run: %q %v", out, err)
	}
}

// TestOrchestrator_GitCommitRequiresRepo checks the upfront validation.
func TestOrchestrator_GitCommitRequiresRepo(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_git_norepo"
	cfg.Goal = "anything"
	cfg.GitCommit = true

	if _, err := NewFromGoal(cfg, &llm.ScriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("expected error for non-git workdir")
	}
}

// TestOrchestrator_StrictPlanFails verifies -strict turns warnings into an error.
func TestOrchestrator_StrictPlanFails(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "test_strict"
	cfg.Goal = "plan quality check"
	cfg.Stream = false
	cfg.Strict = true

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			return &llm.Response{Content: weakPlan}, nil
		},
	}
	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	err = o.RunPlan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "plan check failed: 3 warning(s)") {
		t.Fatalf("expected strict failure, got %v", err)
	}
	// The plan is still saved for inspection / fixing via -resume.
	if _, err := os.Stat(cfg.DataDir + "/" + cfg.SessionID + ".json"); err != nil {
		t.Fatalf("session not saved: %v", err)
	}
}
