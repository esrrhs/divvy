package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunManager_ShutdownDeadlineBranches covers both deadline exits of
// Shutdown with a synthetic never-finishing handle.
func TestRunManager_ShutdownDeadlineBranches(t *testing.T) {
	mgr := NewRunManager()
	mgr.mu.Lock()
	mgr.active["stuck"] = &RunHandle{mgr: mgr, done: make(chan struct{})}
	mgr.mu.Unlock()

	// A grace period already in the past returns before waiting.
	mgr.Shutdown(-1 * time.Second)
	// A short grace expires while waiting for the stuck handle.
	start := time.Now()
	mgr.Shutdown(30 * time.Millisecond)
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("Shutdown must wait out the grace period")
	}
	close(mgr.active["stuck"].done)
}

func TestRunManager_ForgetRules(t *testing.T) {
	mgr := NewRunManager()
	if err := mgr.Forget("missing"); err != nil {
		t.Fatalf("forgetting an unknown session is a no-op, got %v", err)
	}

	live := &RunHandle{mgr: mgr, done: make(chan struct{})}
	live.phase = PhaseRunning
	done := &RunHandle{mgr: mgr, done: make(chan struct{})}
	done.phase = PhaseDone
	mgr.mu.Lock()
	mgr.active["live"] = live
	mgr.active["done"] = done
	mgr.mu.Unlock()

	if err := mgr.Forget("live"); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("forgetting a live run must conflict, got %v", err)
	}
	if err := mgr.Forget("done"); err != nil {
		t.Fatalf("forgetting a terminal run must succeed, got %v", err)
	}
	mgr.mu.Lock()
	_, stillThere := mgr.active["done"]
	mgr.mu.Unlock()
	if stillThere {
		t.Fatal("forgotten handle must be dropped")
	}
}

func TestRunManager_StartValidationErrors(t *testing.T) {
	mgr := NewRunManager()
	// Empty goal.
	if _, err := mgr.Start(Config{WorkDir: t.TempDir()}, &scriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("empty goal must fail, got %v", err)
	}
	// Workdir is a file → NewFromGoal fails and the reservation is released.
	blocker := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := mgrCfg(blocker, t.TempDir(), "test_mgr_badwd")
	cfg.Goal = "g"
	if _, err := mgr.Start(cfg, &scriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("file workdir must fail to start")
	}
	// The failed start must not leave the workdir locked behind.
	cfg2 := mgrCfg(t.TempDir(), t.TempDir(), "test_mgr_after_badwd")
	cfg2.Goal = ""
	if _, err := mgr.Start(cfg2, &scriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "goal is required") {
		t.Fatal("sanity: manager still usable")
	}
}

func TestRunManager_ResumeErrorPaths(t *testing.T) {
	mgr := NewRunManager()
	// Unknown session fails to load.
	cfg := mgrCfg(t.TempDir(), t.TempDir(), "test_mgr_ghost")
	if _, err := mgr.Resume(cfg, &scriptedClient{}, SilentLogger()); err == nil {
		t.Fatal("unknown session must fail to resume")
	}

	// Workdir mismatch: sandbox.Root comes from the unnormalized cfg.WorkDir
	// (trailing space → a distinct directory), while the guard compares the
	// trimmed value against the saved session's workdir. The run is parked at
	// plan review; the mismatch fires before the reservation, so the resume
	// fails cleanly without touching the live handle.
	work, data := t.TempDir(), t.TempDir()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_mismatch"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	mismatch := mgrCfg(work+" ", data, "test_mgr_mismatch")
	mismatch.Goal = ""
	if _, err := mgr.Resume(mismatch, &scriptedClient{}, SilentLogger()); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("workdir mismatch must fail, got %v", err)
	}
	if err := h.Abort(); err != nil {
		t.Fatal(err)
	}

	// Resuming a still-live session conflicts on the active handle. The
	// workdir is booked by the live run, so reserve reports busy first; from
	// a different workdir the session-active guard fires instead.
	gate := make(chan struct{})
	work2, data2 := t.TempDir(), t.TempDir()
	live, err := mgr.Start(mgrCfg(work2, data2, "test_mgr_liveresume"),
		fileWritingScript(mgrAtomicPlan, gate), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, live, PhasePlanReview, 5*time.Second)
	resumeCfg := mgrCfg(work2, data2, "test_mgr_liveresume")
	resumeCfg.Goal = ""
	if _, err := mgr.Resume(resumeCfg, &scriptedClient{}, SilentLogger()); !errors.Is(err, ErrWorkdirBusy) {
		t.Fatalf("resuming a live session from its own workdir must conflict, got %v", err)
	}
	otherDir := mgrCfg(t.TempDir(), data2, "test_mgr_liveresume")
	otherDir.Goal = ""
	if _, err := mgr.Resume(otherDir, &scriptedClient{}, SilentLogger()); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("resuming a live session from another workdir must conflict, got %v", err)
	}
	// The run stays parked at plan review (nobody approves); abort it so the
	// handle reaches a terminal phase and the workdir slot is released.
	if err := live.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := live.Wait(); err == nil {
		t.Fatal("aborted run must report a failure")
	}
}

func TestRunManager_ResumePausedWaitsForWindDown(t *testing.T) {
	// A paused prior handle is awaited before Load reopens the event stream.
	gate := make(chan struct{})
	mgr := NewRunManager()
	work, data := t.TempDir(), t.TempDir()
	h, err := mgr.Start(mgrCfg(work, data, "test_mgr_pausewind"),
		fileWritingScript(mgrAtomicPlan, gate), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhaseRunning, 5*time.Second)
	if err := h.Pause(); err != nil {
		t.Fatal(err)
	}
	// The paused phase is published asynchronously (user_pause event); wait
	// for it so the resume below observes a stable terminal handle.
	waitPhase(t, h, PhasePaused, 5*time.Second)
	close(gate)

	resumeCfg := mgrCfg(work, data, "test_mgr_pausewind")
	resumeCfg.Goal = ""
	resumed, err := mgr.Resume(resumeCfg, fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatalf("resume after pause failed: %v", err)
	}
	waitPhase(t, resumed, PhasePlanReview, 5*time.Second)
	if err := resumed.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Wait(); err != nil {
		t.Fatalf("resumed run failed: %v", err)
	}
	if mgr.Get("test_mgr_pausewind") == nil {
		t.Fatal("terminal handle must stay registered until Forget")
	}
}

func TestRunManager_ActiveSessions(t *testing.T) {
	mgr := NewRunManager()
	mgr.mu.Lock()
	mgr.active["a"] = &RunHandle{mgr: mgr, done: make(chan struct{})}
	mgr.active["b"] = &RunHandle{mgr: mgr, done: make(chan struct{})}
	mgr.mu.Unlock()
	ids := mgr.ActiveSessions()
	if len(ids) != 2 {
		t.Fatalf("ActiveSessions = %v", ids)
	}
	if mgr.Get("a") == nil || mgr.Get("zz") != nil {
		t.Fatal("Get lookup wrong")
	}
}

func TestRunHandle_Accessors(t *testing.T) {
	mgr := NewRunManager()
	h, err := mgr.Start(mgrCfg(t.TempDir(), t.TempDir(), "test_mgr_acc"),
		fileWritingScript(mgrAtomicPlan, nil), SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, h, PhasePlanReview, 5*time.Second)
	if err := h.ApprovePlan(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	if h.Workdir() == "" || h.SessionID() != "test_mgr_acc" {
		t.Fatalf("accessors wrong: %q %q", h.SessionID(), h.Workdir())
	}
	if h.Orchestrator() == nil {
		t.Fatal("Orchestrator accessor broken")
	}
	if got := h.SecretValues(); got != nil {
		t.Fatalf("no api key configured, got %v", got)
	}
	events, cancel := h.SubscribeEvents()
	if events == nil {
		t.Fatal("SubscribeEvents broken")
	}
	cancel()

	// SecretValues reports the configured API key for redaction.
	h.o.cfg.APIKey = "sk-test-secret"
	if got := h.SecretValues(); len(got) != 1 || got[0] != "sk-test-secret" {
		t.Fatalf("SecretValues = %v", got)
	}
}

func TestScrubToolSecrets_MatchesCustomNamedExports(t *testing.T) {
	t.Setenv("MY_CUSTOM_KEY", "the-secret-value")
	cfg := mgrCfg(t.TempDir(), t.TempDir(), "test_scrub")
	cfg.APIKey = "the-secret-value"
	o, err := NewFromGoal(cfg, &scriptedClient{}, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	scrubToolSecrets(o)
	hidden := o.sandbox.HiddenEnv()
	found := false
	for _, k := range hidden {
		if k == "MY_CUSTOM_KEY" {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom-named export not hidden: %v", hidden)
	}
}
