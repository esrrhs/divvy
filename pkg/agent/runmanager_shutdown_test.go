package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A manager interrupted while a leaf runs must checkpoint the tree so the
// session is resumable; the resumed run then completes normally. This is the
// in-process counterpart of the `divvy serve` SIGINT behavior (TR-6.1).
func TestRunManager_ShutdownCheckpointsRunningLeaf(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	mgr := NewRunManagerWithContext(ctx)

	gate := make(chan struct{})
	cfg := mgrCfg(work, data, "test_mgr_shutdown")
	cfg.Goal = "shutdown checkpoint"
	h, err := mgr.Start(cfg, fileWritingScript(mgrAtomicPlan, gate), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseRunning, 5*time.Second)

	// Interrupt like SIGINT, then let the manager wait for checkpoints.
	cancel()
	mgr.Shutdown(10 * time.Second)
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("handle did not finish after shutdown")
	}
	if h.Phase() != PhasePaused {
		t.Fatalf("interrupted run phase=%s, want paused (resumable)", h.Phase())
	}

	// The tree file must exist on disk and survive process restart.
	treePath := filepath.Join(data, h.SessionID()+".json")
	if fi, err := os.Stat(treePath); err != nil || fi.Size() == 0 {
		t.Fatalf("checkpoint tree missing/empty after shutdown: %v", err)
	}

	// Resume in a fresh manager (= a new process): the parked leaf runs
	// again, and this time the gate is open so it finishes.
	mgr2 := NewRunManager()
	h2, err := mgr2.Resume(mgrCfg(work, data, "test_mgr_shutdown"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h2, PhasePlanReview, 5*time.Second)
	if err := h2.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h2.Wait(); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if h2.Phase() != PhaseDone {
		t.Fatalf("resumed phase=%s", h2.Phase())
	}
	if _, err := os.Stat(filepath.Join(work, "out.txt")); err != nil {
		t.Fatalf("resumed run did not produce output: %v", err)
	}
}
