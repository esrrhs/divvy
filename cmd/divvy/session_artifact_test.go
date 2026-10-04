package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written. printSessionArtifact writes straight to os.Stdout.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	runErr := fn()

	w.Close()
	os.Stdout = orig
	out := <-done
	r.Close()
	return out, runErr
}

func writeArtifact(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func baseConfig(dir string) agent.Config {
	return agent.Config{DataDir: dir}
}

// TestPrintSessionArtifactLog covers `-log`: the human-readable run log.
func TestPrintSessionArtifactLog(t *testing.T) {
	dir := t.TempDir()
	logBody := "2026-01-01 step 1\n2026-01-01 step 2\n"
	writeArtifact(t, dir, filepath.Join("logs", "sess1.log"), logBody)

	out, err := captureStdout(t, func() error {
		cfg := baseConfig(dir)
		cfg.SessionID = "sess1"
		return printSessionArtifact(cfg, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != logBody {
		t.Errorf("printed %q, want %q", out, logBody)
	}
}

// TestPrintSessionArtifactEvents covers `-events`: the JSONL trace.
func TestPrintSessionArtifactEvents(t *testing.T) {
	dir := t.TempDir()
	events := `{"kind":"session_start","mode":"run"}
{"kind":"verify","ok":false}
`
	writeArtifact(t, dir, filepath.Join("events", "sess1.jsonl"), events)

	out, err := captureStdout(t, func() error {
		cfg := baseConfig(dir)
		cfg.SessionID = "sess1"
		return printSessionArtifact(cfg, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != events {
		t.Errorf("printed %q, want %q", out, events)
	}
}

// TestPrintSessionArtifactResolvesLatest covers resuming by pointer.
func TestPrintSessionArtifactResolvesLatest(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, filepath.Join("logs", "auto.log"), "latest log\n")
	if err := os.WriteFile(filepath.Join(dir, "LATEST"), []byte("auto\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return printSessionArtifact(baseConfig(dir), false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "latest log\n" {
		t.Errorf("printed %q; the LATEST pointer should have been resolved", out)
	}
}

// TestPrintSessionArtifactDistinguishesLogAndEvents guards against reading the
// wrong artifact: a session with both files must print the requested one.
func TestPrintSessionArtifactDistinguishesLogAndEvents(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, filepath.Join("logs", "s.log"), "LOG\n")
	writeArtifact(t, dir, filepath.Join("events", "s.jsonl"), "EVENTS\n")

	logOut, err := captureStdout(t, func() error {
		cfg := baseConfig(dir)
		cfg.SessionID = "s"
		return printSessionArtifact(cfg, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if logOut != "LOG\n" {
		t.Errorf("-log printed %q", logOut)
	}

	evOut, err := captureStdout(t, func() error {
		cfg := baseConfig(dir)
		cfg.SessionID = "s"
		return printSessionArtifact(cfg, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if evOut != "EVENTS\n" {
		t.Errorf("-events printed %q", evOut)
	}
}

func TestPrintSessionArtifactErrors(t *testing.T) {
	t.Run("no session and no LATEST", func(t *testing.T) {
		err := printSessionArtifact(baseConfig(t.TempDir()), false)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no -session given") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("missing log file names the session", func(t *testing.T) {
		dir := t.TempDir()
		cfg := baseConfig(dir)
		cfg.SessionID = "ghost"
		err := printSessionArtifact(cfg, false)
		if err == nil {
			t.Fatal("expected an error")
		}
		// The message must be actionable, not just "no such file".
		if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "-sessions") {
			t.Errorf("error should name the session and suggest -sessions, got %v", err)
		}
		if !strings.Contains(err.Error(), "run log") {
			t.Errorf("error should say which artifact is missing, got %v", err)
		}
	})

	t.Run("missing event file says event stream", func(t *testing.T) {
		dir := t.TempDir()
		cfg := baseConfig(dir)
		cfg.SessionID = "ghost"
		err := printSessionArtifact(cfg, true)
		if err == nil || !strings.Contains(err.Error(), "event stream") {
			t.Errorf("err = %v, want it to mention the event stream", err)
		}
	})

	t.Run("log exists but events do not", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifact(t, dir, filepath.Join("logs", "s.log"), "x")
		cfg := baseConfig(dir)
		cfg.SessionID = "s"
		if err := printSessionArtifact(cfg, true); err == nil {
			t.Fatal("expected an error for the missing event stream")
		}
	})
}

// TestRunLogFlagEndToEnd drives the flag through run() rather than calling the
// helper directly, covering the dispatch path.
func TestRunLogFlagEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, filepath.Join("logs", "sess1.log"), "hello from the log\n")

	out, err := captureStdout(t, func() error {
		return run([]string{
			"-log",
			"-session", "sess1",
			"-datadir", dir,
			"-workdir", t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello from the log") {
		t.Errorf("-log did not print the log, got %q", out)
	}
}

func TestRunEventsFlagEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, filepath.Join("events", "sess1.jsonl"), `{"kind":"verify","ok":false}`+"\n")

	out, err := captureStdout(t, func() error {
		return run([]string{
			"-events",
			"-session", "sess1",
			"-datadir", dir,
			"-workdir", t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"kind":"verify"`) {
		t.Errorf("-events did not print the event stream, got %q", out)
	}
}

// TestRunSessionsFlagOnEmptyDir covers the empty-listing path. It writes the
// tree file directly rather than running -plan, because the planning path
// would make real (and retried) network calls.
func TestRunSessionsFlagOnEmptyDir(t *testing.T) {
	work := t.TempDir()
	out, err := captureStdout(t, func() error {
		return run([]string{"-sessions", "-datadir", t.TempDir(), "-workdir", work})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no saved sessions") {
		t.Errorf("expected an empty-listing message, got %q", out)
	}
}

// TestRunSessionsFlagListsRealTrees writes a valid session tree and checks the
// listing renders a row for it.
func TestRunSessionsFlagListsRealTrees(t *testing.T) {
	data := t.TempDir()
	tree := engine.NewTaskTree("listed", "ship the thing", "desc")
	if _, err := tree.AddChild(tree.RootID, "leaf1", "leaf1", "d",
		models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode(tree.RootID, func(n *models.TaskNode) error {
		n.State = models.TaskStateRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := tree.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "listed.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return run([]string{"-sessions", "-datadir", data, "-workdir", t.TempDir()})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SESSION") {
		t.Errorf("expected a table header, got %q", out)
	}
	if !strings.Contains(out, "listed") {
		t.Errorf("expected the session id in the listing, got %q", out)
	}
	if !strings.Contains(out, "ship the thing") {
		t.Errorf("expected the goal in the listing, got %q", out)
	}
}

// TestRunStatusWithoutSessionFails covers the error path when -status cannot
// resolve a session.
func TestRunStatusWithoutSessionFails(t *testing.T) {
	err := run([]string{
		"-status",
		"-datadir", t.TempDir(),
		"-workdir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error with no LATEST pointer")
	}
	if !strings.Contains(err.Error(), "LATEST") {
		t.Errorf("error should mention the missing LATEST pointer, got %v", err)
	}
}

// TestRunRejectsHelp cleanly returns rather than erroring on -h.
func TestRunRejectsHelp(t *testing.T) {
	if err := run([]string{"-h"}); err != nil {
		t.Errorf("-h should exit cleanly, got %v", err)
	}
}
