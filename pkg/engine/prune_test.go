package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newPruneFixture writes three sessions with distinct update times (s3 newest)
// plus a log and event file for each, then points LATEST at latest.
func newPruneFixture(t *testing.T, latest string) *Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"s1", "s2", "s3"} {
		tree := NewTaskTree(id, "goal "+id, "goal "+id)
		// Distinct, deterministic ordering: s3 is newest.
		tree.UpdatedAt = time.Now().Add(-time.Duration(3-i) * time.Hour)
		if err := s.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
		for _, sub := range []string{"logs", "events"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
			name := id + ".jsonl"
			if sub == "logs" {
				name = id + ".log"
			}
			if err := os.WriteFile(filepath.Join(dir, sub, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if latest != "" {
		if err := os.WriteFile(filepath.Join(dir, "LATEST"), []byte(latest+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestStorage_PlanPruneKeepsNewest(t *testing.T) {
	s := newPruneFixture(t, "s3")
	plan, err := s.PlanPrune(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removed) != 1 || plan.Removed[0].ID != "s1" {
		t.Fatalf("should drop only the oldest, got %+v", plan.Removed)
	}
	// The tree plus its log and event stream all belong to the session.
	if len(plan.Removed[0].Paths) != 3 {
		t.Fatalf("prune should cover tree+log+events, got %v", plan.Removed[0].Paths)
	}
	if len(plan.Kept) != 2 {
		t.Fatalf("kept %v, want 2", plan.Kept)
	}
	if plan.Bytes <= 0 {
		t.Fatal("prune should report the bytes it frees")
	}
}

// TestStorage_PlanPruneNeverDropsLatest guards the one session a bare
// `-resume` would pick up: pruning it would silently break the workflow.
func TestStorage_PlanPruneNeverDropsLatest(t *testing.T) {
	s := newPruneFixture(t, "s1") // LATEST is the oldest session
	plan, err := s.PlanPrune(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removed) != 0 {
		t.Fatalf("LATEST session must be exempt, got %+v", plan.Removed)
	}
	if len(plan.Kept) != 3 {
		t.Fatalf("kept %v, want all 3", plan.Kept)
	}
}

func TestStorage_PruneSessionsDeletesArtifacts(t *testing.T) {
	s := newPruneFixture(t, "s3")
	res, err := s.PruneSessions(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 {
		t.Fatalf("removed %+v", res.Removed)
	}
	dir := s.baseDir
	for _, name := range []string{"s1.json", filepath.Join("logs", "s1.log"), filepath.Join("events", "s1.jsonl")} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone (err=%v)", name, err)
		}
	}
	// Surviving sessions keep everything, including their logs and events.
	for _, id := range []string{"s2", "s3"} {
		if !s.TreeExists(id) {
			t.Fatalf("%s should survive", id)
		}
		if _, err := os.Stat(filepath.Join(dir, "logs", id+".log")); err != nil {
			t.Fatalf("%s log should survive: %v", id, err)
		}
	}
	if _, err := s.LoadTree("s1"); err == nil {
		t.Fatal("s1 should no longer be loadable")
	}
}

func TestStorage_PlanPruneRejectsKeepBelowOne(t *testing.T) {
	s := newPruneFixture(t, "s3")
	if _, err := s.PlanPrune(0); err == nil {
		t.Fatal("keep=0 must be rejected: it would delete the session in use")
	}
	if _, err := s.PruneSessions(-1); err == nil {
		t.Fatal("negative keep must be rejected")
	}
}
