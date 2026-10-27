package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
)

// TestRun_NormalRunRequiresAPIKey covers the missing-key guard on the batch
// (non-interactive) path.
func TestRun_NormalRunRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("LLM_API_KEY", "")
	err := run([]string{"-workdir", t.TempDir(), "-datadir", t.TempDir(),
		"-base-url", "https://api.example.com/v1", "goal"})
	if err == nil || !strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("batch run without a key must fail, got %v", err)
	}
}

func TestRun_ReportMissingSessionFails(t *testing.T) {
	err := run([]string{"-datadir", t.TempDir(), "-report"})
	if err == nil {
		t.Fatal("report on an empty data dir must fail")
	}
}

func TestRun_PruneStorageError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "datablock")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-datadir", blocker, "-prune"}); err == nil {
		t.Fatal("prune with an unusable data dir must fail")
	}
}

// TestRun_SessionsTruncatesLongGoals pins the 60-char goal column.
func TestRun_SessionsTruncatesLongGoals(t *testing.T) {
	dir := t.TempDir()
	storage, err := engine.NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("g", 90)
	tree := engine.NewTaskTree("longgoal", long, long)
	if err := storage.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return run([]string{"-datadir", dir, "-sessions"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "g...") {
		t.Fatalf("long goal not truncated:\n%s", out)
	}
	if strings.Count(out, "gggggggggg") > 7 {
		t.Fatalf("goal printed untruncated:\n%s", out)
	}
}

func TestRun_ExplicitIsolateFlagWins(t *testing.T) {
	// The flag is parsed before validation fails, so a rejected run still
	// proves the explicit-isolate branch executed.
	err := run([]string{"-workdir", t.TempDir(), "-datadir", t.TempDir(),
		"-isolate=false", "goal without a key"})
	if err == nil || !strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("expected the key guard, got %v", err)
	}
}

// TestRun_InteractiveEOFEndsCleanly drives the REPL path to an immediate
// stdin EOF (the test process has no terminal input).
func TestRun_InteractiveEOFEndsCleanly(t *testing.T) {
	err := run([]string{"-interactive", "-workdir", t.TempDir(),
		"-datadir", t.TempDir(), "-base-url", "http://127.0.0.1:9/v1"})
	if err != nil {
		t.Fatalf("EOF should end the interactive session cleanly, got %v", err)
	}
}

// TestRun_GuidedEOFBeforeApprovalSavesPlan drives the guided path to plan
// review and ends at EOF; the plan must be saved for -resume.
func TestRun_GuidedEOFBeforeApprovalSavesPlan(t *testing.T) {
	srv := stubDivvyEndpoint(t)
	defer srv.Close()

	work := t.TempDir()
	data := t.TempDir()
	err := run([]string{
		"-guided", "-workdir", work, "-datadir", data,
		"-base-url", srv.URL, "-no-stream",
		"write hello.txt",
	})
	if err != nil {
		t.Fatalf("guided EOF must end cleanly, got %v", err)
	}
	// The decomposed plan was persisted: LATEST points at a saved tree.
	latest, err := os.ReadFile(filepath.Join(data, "LATEST"))
	if err != nil {
		t.Fatalf("guided run did not save the plan: %v", err)
	}
	storage, err := engine.NewStorage(data)
	if err != nil {
		t.Fatal(err)
	}
	if !storage.TreeExists(strings.TrimSpace(string(latest))) {
		t.Fatalf("LATEST names session %q but its tree is missing", latest)
	}
}

// TestRun_GoalRunSurfacesFailure covers the error path of a completed-but-
// failing run: an unsatisfiable DoD with a 1ms leaf time budget gives up,
// the run returns an error, and main logs the resume hint.
func TestRun_GoalRunSurfacesFailure(t *testing.T) {
	srv := stubFailingEndpoint(t)
	defer srv.Close()

	err := run([]string{
		"-workdir", t.TempDir(), "-datadir", t.TempDir(),
		"-base-url", srv.URL, "-no-stream",
		"-max-elapsed", "1ms",
		"impossible goal",
	})
	if err == nil {
		t.Fatal("a run whose leaf gave up must return an error")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("unexpected run error: %v", err)
	}
}

// stubFailingEndpoint plans an atomic leaf whose DoD command always fails;
// the worker then writes and finishes, and verification can never pass.
func stubFailingEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	plan := mustJSONDivvy(map[string]any{
		"is_atomic": true,
		"reason":    "tiny",
		"contract": map[string]any{
			"inputs": []string{}, "outputs": []string{"out.txt"},
			"dependencies": []string{}, "constraints": []string{},
		},
		"dod": map[string]any{
			"description": "never passes",
			"commands":    []string{"false"},
			"timeout_sec": 30,
		},
	})
	step := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sys := ""
		if len(body.Messages) > 0 {
			sys = body.Messages[0].Content
		}
		content := plan
		if !strings.Contains(sys, "software architect") {
			step++
			if step == 1 {
				content = mustJSONDivvy(map[string]any{
					"action": "write_file",
					"args":   map[string]any{"path": "out.txt", "content": "x\n"},
				})
			} else {
				content = mustJSONDivvy(map[string]any{
					"action": "finish",
					"args":   map[string]any{"summary": "done"},
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message":       map[string]any{"content": content},
			}},
			"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12},
		})
	}))
}
