package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
)

func TestRun_ServeMutexDetails(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"goal argument", []string{"-serve", "some goal"}, "-serve takes no goal argument"},
		{"with status", []string{"-serve", "-status"}, "-serve is mutually exclusive with -status"},
		{"with report", []string{"-serve", "-report"}, "-serve is mutually exclusive with -report"},
		{"with prune", []string{"-serve", "-prune"}, "-serve is mutually exclusive with -prune"},
		{"with resume", []string{"-serve", "-resume"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(append([]string{"-workdir", t.TempDir(), "-datadir", t.TempDir()}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%v) = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestRun_InteractiveAndGuidedRequireAPIKey covers the missing-key guard for
// both human-in-the-loop modes. A non-loopback base URL forces the key
// requirement regardless of how the developer's environment is configured;
// the env keys are emptied so the guard cannot pass by accident.
func TestRun_InteractiveAndGuidedRequireAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("LLM_API_KEY", "")
	for _, mode := range []string{"-interactive", "-guided"} {
		err := run([]string{mode, "-workdir", t.TempDir(), "-datadir", t.TempDir(),
			"-base-url", "https://api.example.com/v1"})
		if err == nil || !strings.Contains(err.Error(), "missing API key") {
			t.Fatalf("%s without a key must fail, got %v", mode, err)
		}
	}
}

// TestRun_InteractiveTempDatadir proves an interactive session without an
// explicit -datadir keeps its storage out of the workspace (the temp-dir
// branch runs before the API-key guard, so the error still proves the branch).
func TestRun_InteractiveTempDatadir(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("LLM_API_KEY", "")
	work := t.TempDir()
	err := run([]string{"-interactive", "-workdir", work,
		"-base-url", "https://api.example.com/v1"})
	if err == nil || !strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("expected the key guard, got %v", err)
	}
	entries, _ := os.ReadDir(work)
	if len(entries) != 0 {
		t.Fatalf("interactive run must not touch the workspace, found %v", entries)
	}
}

func TestRun_PruneActuallyDeletes(t *testing.T) {
	dir := t.TempDir()
	storage, err := engine.NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"old1", "old2", "keep1", "keep2"} {
		tree := engine.NewTaskTree(id, "goal "+id, "goal "+id)
		tree.UpdatedAt = time.Now().Add(-time.Duration(4-i) * time.Hour)
		if err := storage.SaveTree(tree); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "LATEST"), []byte("keep2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return run([]string{"-datadir", dir, "-prune", "-keep", "2"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old1", "old2"} {
		if storage.TreeExists(id) {
			t.Fatalf("prune should have deleted %s", id)
		}
	}
	for _, id := range []string{"keep1", "keep2"} {
		if !storage.TreeExists(id) {
			t.Fatalf("prune deleted a kept session: %s", id)
		}
	}
	if !strings.Contains(out, "deleted") || !strings.Contains(out, "2 kept") {
		t.Fatalf("prune summary unexpected:\n%s", out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "LATEST")); string(data) != "keep2\n" {
		t.Fatalf("LATEST must survive pruning, got %q", data)
	}
}

func TestPruneSessions_NothingToDo(t *testing.T) {
	dir := t.TempDir()
	out, err := captureStdout(t, func() error {
		return pruneSessions(baseConfig(dir), 5, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to prune") {
		t.Fatalf("expected the nothing-to-prune message, got:\n%s", out)
	}
}

func TestRun_StatusPrintsSavedTree(t *testing.T) {
	dir := t.TempDir()
	storage, err := engine.NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.NewTaskTree("status-sess", "the goal", "the goal")
	if err := storage.SaveTree(tree); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return run([]string{"-datadir", dir, "-session", "status-sess", "-status"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "status-sess") || !strings.Contains(out, "the goal") {
		t.Fatalf("status output missing tree details:\n%s", out)
	}
}

func TestHumanBytes_Renders(t *testing.T) {
	cases := map[int64]string{
		512:                    "512 B",
		2048:                   "2.0 KiB",
		5 * 1024 * 1024:        "5.0 MiB",
		3 * 1024 * 1024 * 1024: "3.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRun_UnknownFlagIsError(t *testing.T) {
	err := run([]string{"-definitely-not-a-flag"})
	if err == nil {
		t.Fatal("unknown flag must error")
	}
}

// stubDivvyEndpoint serves scripted non-streaming completions: the architect
// gets an atomic plan, the worker writes hello.txt and then finishes.
func stubDivvyEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
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
		content := ""
		if strings.Contains(sys, "software architect") {
			content = mustJSONDivvy(map[string]any{
				"is_atomic": true,
				"reason":    "one file",
				"contract": map[string]any{
					"inputs": []string{}, "outputs": []string{"hello.txt"},
					"dependencies": []string{}, "constraints": []string{},
				},
				"dod": map[string]any{
					"description": "hello.txt exists",
					"commands":    []string{"test -f hello.txt"},
					"timeout_sec": 30,
				},
			})
		} else {
			step++
			if step == 1 {
				content = mustJSONDivvy(map[string]any{
					"action": "write_file",
					"args":   map[string]any{"path": "hello.txt", "content": "hello divvy\n"},
				})
			} else {
				content = mustJSONDivvy(map[string]any{
					"action": "finish",
					"args":   map[string]any{"summary": "wrote hello.txt"},
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

func mustJSONDivvy(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestRun_GoalEndToEndWithStub(t *testing.T) {
	srv := stubDivvyEndpoint(t)
	defer srv.Close()

	work := t.TempDir()
	err := run([]string{
		"-workdir", work, "-datadir", t.TempDir(),
		"-base-url", srv.URL, "-no-stream",
		"create hello.txt saying hello",
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "hello.txt"))
	if err != nil || string(data) != "hello divvy\n" {
		t.Fatalf("hello.txt wrong: %q %v", data, err)
	}
}

func TestRun_PlanEndToEndWithStub(t *testing.T) {
	srv := stubDivvyEndpoint(t)
	defer srv.Close()

	work := t.TempDir()
	err := run([]string{
		"-workdir", work, "-datadir", t.TempDir(),
		"-base-url", srv.URL, "-no-stream", "-plan",
		"plan only",
	})
	if err != nil {
		t.Fatalf("plan run failed: %v", err)
	}
}
