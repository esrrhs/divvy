package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/llm"
)

// stubChatEndpoint serves a minimal OpenAI-compatible /chat/completions API
// (non-streaming, the mode the bench uses) driven by the same scripted plan
// and worker actions as TestBench_GoalRunEndToEnd.
func stubChatEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	step := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sys := ""
		if len(body.Messages) > 0 {
			sys = body.Messages[0].Content
		}
		content := ""
		if strings.Contains(sys, "software architect") {
			content = mustJSON(map[string]any{
				"is_atomic": true,
				"reason":    "single package",
				"contract": map[string]any{
					"inputs": []string{}, "outputs": []string{"strutil.go", "strutil_test.go"},
					"dependencies": []string{}, "constraints": []string{},
				},
				"dod": map[string]any{
					"description": "tests pass",
					"commands":    []string{"go build ./...", "go test ./..."},
					"timeout_sec": 120,
				},
			})
		} else {
			step++
			switch step {
			case 1, 2:
				paths := []string{"strutil.go", "strutil_test.go"}
				action := map[string]any{
					"action": "write_file",
					"args": map[string]any{
						"path":    paths[step-1],
						"content": "package strutil\n",
					},
				}
				if step == 2 {
					action["args"].(map[string]any)["content"] = `package strutil

import "testing"

func TestNothing(t *testing.T) {}
`
				}
				content = mustJSON(action)
			default:
				content = mustJSON(map[string]any{
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
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
}

func TestBench_RunListFlag(t *testing.T) {
	if err := run([]string{"-list"}); err != nil {
		t.Fatalf("-list should succeed: %v", err)
	}
	if corpusNames() == "" || !strings.Contains(corpusNames(), "string-utils") {
		t.Fatalf("corpusNames unexpected: %q", corpusNames())
	}
	if firstLine("a\nb") != "a" || firstLine("solo") != "solo" {
		t.Fatal("firstLine broken")
	}
}

func TestBench_RunValidationErrors(t *testing.T) {
	cases := [][]string{
		{"-goals", "nope"},
		{"-repeat", "0"},
		{"-max-steps", "0"},
		{"-max-retries", "0"},
		{"-bogus-flag"},
	}
	for _, args := range cases {
		if err := run(args); err == nil {
			t.Errorf("run(%v) should fail", args)
		}
	}
}

func TestBench_RunEndToEndWithStubEndpoint(t *testing.T) {
	srv := stubChatEndpoint(t)
	defer srv.Close()

	root := t.TempDir()
	out := filepath.Join(root, "report.json")
	err := run([]string{
		"-base-url", srv.URL + "/v1",
		"-model", "gpt-4o-mini", // priced: exercises the cost accounting branch
		"-goals", "string-utils",
		"-root", filepath.Join(root, "runs"),
		"-out", out,
		"-timeout", "3m",
		"-repeat", "1",
	})
	if err != nil {
		t.Fatalf("bench run failed: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	var rep benchReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report JSON invalid: %v", err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Goal != "string-utils" || !rep.Results[0].OK {
		t.Fatalf("unexpected report: %+v", rep.Results)
	}
	if rep.Results[0].CostUSD <= 0 {
		t.Fatalf("priced model must produce a cost: %+v", rep.Results[0])
	}
	if rep.Aggregate.Runs != 1 || rep.Aggregate.OK != 1 {
		t.Fatalf("aggregate wrong: %+v", rep.Aggregate)
	}
	if rep.Meta.Model != "gpt-4o-mini" || rep.Meta.Finished.IsZero() {
		t.Fatalf("meta wrong: %+v", rep.Meta)
	}
}

// TestBench_RunDefaultReportPath covers the -out default and the goal-loop
// repeat bookkeeping.
func TestBench_RunDefaultReportPath(t *testing.T) {
	srv := stubChatEndpoint(t)
	defer srv.Close()

	root := t.TempDir()
	err := run([]string{
		"-base-url", srv.URL + "/v1",
		"-model", "stub",
		"-goals", "string-utils",
		"-root", filepath.Join(root, "runs"),
		"-timeout", "3m",
	})
	if err != nil {
		t.Fatalf("bench run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runs", "report.json")); err != nil {
		t.Fatalf("default report path not used: %v", err)
	}
}

// TestBench_RunRootCreationError covers the mkdir failure of the bench root.
func TestBench_RunRootCreationError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-goals", "string-utils", "-root", filepath.Join(blocker, "sub")}); err == nil ||
		!strings.Contains(err.Error(), "create bench root") {
		t.Fatalf("file-root bench must fail, got %v", err)
	}
}

func TestBench_RunGoalErrorPaths(t *testing.T) {
	// A goal whose workspace seed fails (root is a file) is a data point.
	fileRoot := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(fileRoot, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	g := Goal{Name: "boom", Prompt: "p", Seed: map[string]string{"a/b.txt": "x"}, Module: "m"}
	bc := benchConfig{Root: fileRoot, Timeout: time.Second, MaxSteps: 1, MaxRetries: 1,
		newClient: func(agent.Config) llm.Client { return &llm.ScriptedClient{} }}
	res := runGoal(context.Background(), g, 1, bc)
	if res.OK || !strings.Contains(res.Error, "seed workspace") {
		t.Fatalf("expected seed error, got %+v", res)
	}

	// One-liner truncation keeps error messages bounded.
	if got := oneLine(strings.Repeat("x", 200)); len(got) != 93 || !strings.HasSuffix(got, "...") {
		t.Fatalf("oneLine = %d chars", len(got))
	}
}

func TestBench_AggregateEmptyAndCost(t *testing.T) {
	if agg := aggregate(nil); agg.Runs != 0 || agg.SuccessRate != 0 {
		t.Fatalf("empty aggregate = %+v", agg)
	}
	res := runGoal // keep the symbol referenced
	_ = res
	p := cost.DefaultPricing()
	if _, ok := p.PriceFor("nonexistent-model"); ok {
		t.Fatal("unknown model should have no price")
	}
}
