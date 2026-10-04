package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
)

func TestRun_RejectsInteractiveAndGuided(t *testing.T) {
	err := run([]string{
		"-interactive", "-guided",
		"-workdir", t.TempDir(),
		"-datadir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error when -interactive and -guided are both set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusion error, got %v", err)
	}
}

// TestRun_RejectsNegativeBudgetFlags covers the guardrails that stop a
// nonsensical budget from silently disabling a limit the operator intended.
func TestRun_RejectsNegativeBudgetFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"negative max-elapsed", []string{"-max-elapsed", "-5s"}, "-max-elapsed must be >= 0"},
		{"negative max-retries", []string{"-max-retries", "-1"}, "-max-retries must be >= 0"},
		{"negative max-cost", []string{"-max-cost", "-1"}, "-max-cost must be >= 0"},
		{"negative budget-tokens", []string{"-budget-tokens", "-1"}, "-budget-tokens must be >= 0"},
		{"negative max-stall", []string{"-max-stall", "-1"}, "-max-stall must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"-workdir", t.TempDir(),
				"-datadir", t.TempDir(),
				"-api-key", "dummy",
			}, tc.args...)
			args = append(args, "goal")
			err := run(args)
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestRun_PruneDryRunKeepsEverything is the safety property that matters most
// for a destructive flag: `-dry-run` must report the same set of sessions a
// real prune would delete and leave every file on disk.
func TestRun_PruneDryRunKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	storage, err := engine.NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"s1", "s2", "s3"} {
		tree := engine.NewTaskTree(id, "goal "+id, "goal "+id)
		tree.UpdatedAt = time.Now().Add(-time.Duration(3-i) * time.Hour)
		if err := storage.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "LATEST"), []byte("s3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"-datadir", dir, "-prune", "-keep", "2", "-dry-run"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if !storage.TreeExists(id) {
			t.Fatalf("-dry-run deleted session %s", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "LATEST")); err != nil {
		t.Fatalf("-dry-run removed the LATEST pointer: %v", err)
	}
}

// TestRun_RejectsKeepBelowOne: `-keep 0` would delete every session including
// the one in use, so it must never be accepted.
func TestRun_RejectsKeepBelowOne(t *testing.T) {
	err := run([]string{"-datadir", t.TempDir(), "-prune", "-keep", "0"})
	if err == nil {
		t.Fatal("expected -keep 0 to be rejected")
	}
	if !strings.Contains(err.Error(), "keep must be >= 1") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRun_ReportPrintsSummary exercises the -report wiring end to end: the
// session is written by hand (no LLM), then read back through the CLI.
func TestRun_ReportPrintsSummary(t *testing.T) {
	dir := t.TempDir()
	storage, err := engine.NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.NewTaskTree("sess_report", "ship the thing", "ship the thing")
	if _, err := tree.AddChild(tree.RootID, "1.1", "first leaf", "do it", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode("1.1", func(n *models.TaskNode) error {
		n.ErrorMsg = "boom"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LATEST"), []byte("sess_report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return run([]string{"-datadir", dir, "-workdir", t.TempDir(), "-report"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sess_report", "ship the thing", "failures", "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("-report output is missing %q:\n%s", want, out)
		}
	}
}

// TestRun_RequiresGoalOrResume keeps the two entry paths distinct.
func TestRun_RequiresGoalOrResume(t *testing.T) {
	err := run([]string{
		"-workdir", t.TempDir(),
		"-datadir", t.TempDir(),
		"-api-key", "dummy",
	})
	if err == nil {
		t.Fatal("expected an error when neither a goal nor -resume is given")
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
