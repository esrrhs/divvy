package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/llm"
)

// scriptedGoalClient drives a full goal run without a model: it answers the
// decomposer with an atomic plan and the worker with two write_file actions
// plus finish, so the harness machinery (seeding, go mod init, git baseline,
// real go build/go test verification, metric collection) is exercised end to
// end. The scripted code is the same shape a competent weak model would emit.
func scriptedGoalClient() llm.Client {
	workerStep := 0
	return &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		if strings.Contains(sys, "software architect") {
			return &llm.Response{
				Content: mustJSON(map[string]any{
					"is_atomic": true,
					"reason":    "one package with three small functions and tests",
					"contract": map[string]any{
						"inputs":       []string{},
						"outputs":      []string{"strutil.go", "strutil_test.go"},
						"dependencies": []string{},
						"constraints":  []string{"standard library only"},
					},
					"dod": map[string]any{
						"description": "strutil builds and its unit tests pass",
						"commands":    []string{"go build ./...", "go test ./..."},
						"timeout_sec": 120,
					},
				}),
				Usage: llm.Usage{PromptTokens: 400, CompletionTokens: 120},
			}, nil
		}
		workerStep++
		var action map[string]any
		switch workerStep {
		case 1:
			action = map[string]any{
				"thought": "write the package",
				"action":  "write_file",
				"args": map[string]any{
					"path": "strutil.go",
					"content": `package strutil

import "strings"

// Reverse returns s with its runes in reverse order.
func Reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// IsPalindrome reports whether s reads the same forwards and backwards,
// comparing only letters and digits and ignoring case.
func IsPalindrome(s string) bool {
	var kept []rune
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			kept = append(kept, c)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		if kept[i] != kept[j] {
			return false
		}
	}
	return true
}

// WordCount counts whitespace-separated words in s.
func WordCount(s string) int {
	return len(strings.Fields(s))
}
`,
				},
			}
		case 2:
			action = map[string]any{
				"thought": "write the table-driven tests",
				"action":  "write_file",
				"args": map[string]any{
					"path": "strutil_test.go",
					"content": `package strutil

import "testing"

func TestReverse(t *testing.T) {
	cases := map[string]string{"": "", "abc": "cba", "你好": "好你"}
	for in, want := range cases {
		if got := Reverse(in); got != want {
			t.Errorf("Reverse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsPalindrome(t *testing.T) {
	cases := map[string]bool{"": true, "No lemon, no melon": true, "hello": false, "12321": true}
	for in, want := range cases {
		if got := IsPalindrome(in); got != want {
			t.Errorf("IsPalindrome(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWordCount(t *testing.T) {
	cases := map[string]int{"": 0, "one two three": 3, "  padded  words  ": 2}
	for in, want := range cases {
		if got := WordCount(in); got != want {
			t.Errorf("WordCount(%q) = %d, want %d", in, got, want)
		}
	}
}
`,
				},
			}
		default:
			action = map[string]any{
				"thought": "package and tests are in place",
				"action":  "finish",
				"args":    map[string]any{"summary": "implemented strutil with tests"},
			}
		}
		return &llm.Response{
			Content: mustJSON(action),
			Usage:   llm.Usage{PromptTokens: 300, CompletionTokens: 150},
		}, nil
	}}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestBench_GoalRunEndToEnd runs the string-utils corpus goal through the
// real orchestrator with a scripted model: the harness must seed a working
// Go workspace, drive the goal to a verified COMPLETED root, and collect
// tokens/counters. This is the machinery guard — if it passes, a real bench
// failure is a model failure, not a harness failure.
func TestBench_GoalRunEndToEnd(t *testing.T) {
	g, ok := goalByName("string-utils")
	if !ok {
		t.Fatal("corpus is missing the string-utils goal")
	}
	bc := benchConfig{
		Model:      "scripted",
		MaxSteps:   10,
		MaxRetries: 1,
		Timeout:    3 * time.Minute,
		Root:       t.TempDir(),
		Pricing:    cost.DefaultPricing(),
		newClient:  func(agent.Config) llm.Client { return scriptedGoalClient() },
	}

	res := runGoal(context.Background(), g, 1, bc)
	if !res.OK {
		t.Fatalf("goal run failed: ok=%v error=%q (workspace %s)", res.OK, res.Error, res.Workdir)
	}
	for _, f := range []string{"strutil.go", "strutil_test.go"} {
		if _, err := os.Stat(filepath.Join(res.Workdir, f)); err != nil {
			t.Fatalf("expected %s in the workspace: %v", f, err)
		}
	}
	if res.LLMCalls < 3 {
		t.Errorf("LLMCalls = %d, want >= 3 (plan + 2 writes + finish)", res.LLMCalls)
	}
	if res.TotalTokens <= 0 || res.PromptTokens <= 0 || res.CompletionTokens <= 0 {
		t.Errorf("token accounting missing: %+v", res)
	}
	if res.Leaves != 1 || res.LeavesDone != 1 {
		t.Errorf("leaves = %d/%d, want 1/1", res.LeavesDone, res.Leaves)
	}
	if res.SessionID == "" || res.WallMS <= 0 {
		t.Errorf("session/wall accounting missing: %+v", res)
	}
}

// TestBench_SeedWorkspaceIsolomorphic checks the fix-average corpus really
// contains a failing bug (the bench is worthless if its goals are broken):
// the seeded test must fail against the seeded implementation.
func TestBench_SeedWorkspaceIsolomorphic(t *testing.T) {
	g, ok := goalByName("fix-average")
	if !ok {
		t.Fatal("corpus is missing the fix-average goal")
	}
	dir := t.TempDir()
	if err := g.seedWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("go.mod not seeded: %v", err)
	}
	out, err := runCmd(dir, "go", "test", "./...")
	if err == nil {
		t.Fatal("seeded workspace must fail its tests (otherwise the goal is not a fix task)")
	}
	if !strings.Contains(string(out), "Average") {
		t.Fatalf("expected the failure to name Average, got:\n%s", out)
	}
}

// TestBench_ReportRenders locks the report shape: every run appears as one
// row and the aggregate math is consistent.
func TestBench_ReportRenders(t *testing.T) {
	rs := []RunResult{
		{Goal: "a", Repeat: 1, OK: true, WallMS: 1500, LLMCalls: 6, TotalTokens: 100, Leaves: 1, LeavesDone: 1},
		{Goal: "a", Repeat: 2, OK: false, Error: "boom", WallMS: 2500, LLMCalls: 4, TotalTokens: 60},
	}
	table := renderTable(rs)
	if strings.Count(table, "\n") != 3 { // header + 2 rows
		t.Fatalf("table rows:\n%s", table)
	}
	if !strings.Contains(table, "boom") {
		t.Error("failed run's error must be visible")
	}
	agg := aggregate(rs)
	if agg.Runs != 2 || agg.OK != 1 || agg.SuccessRate != 0.5 {
		t.Errorf("aggregate = %+v", agg)
	}
	if agg.TotalTokens != 160 || agg.LLMCalls != 10 || agg.WallMS != 4000 {
		t.Errorf("aggregate sums = %+v", agg)
	}
}

// TestBench_SelectGoalsFilters keeps -goals filtering honest.
func TestBench_SelectGoalsFilters(t *testing.T) {
	if got := selectGoals(""); len(got) != len(Corpus) {
		t.Errorf("empty filter should select all %d goals, got %d", len(Corpus), len(got))
	}
	got := selectGoals("fix-average, http-health")
	if len(got) != 2 || got[0].Name != "fix-average" || got[1].Name != "http-health" {
		t.Fatalf("filtered corpus = %+v", got)
	}
	if got := selectGoals("nope"); len(got) != 0 {
		t.Errorf("unknown goal should select nothing, got %+v", got)
	}
}

// scriptedDirectClient drives the direct loop without a model: two writes then
// finish, the same shape the tree test's scripted worker uses. It never asks
// for shell execution, so the harness itself can be exercised on hosts without
// an isolation backend (the acceptance step then reports "not run").
func scriptedDirectClient() llm.Client {
	step := 0
	return &llm.ScriptedClient{Handle: func(_ context.Context, req llm.Request) (*llm.Response, error) {
		step++
		var action map[string]any
		switch step {
		case 1:
			action = map[string]any{
				"thought": "create the package",
				"action":  "write_file",
				"args": map[string]any{
					"path": "strutil.go",
					"content": "package strutil\n\nimport \"strings\"\n\n" +
						"// Reverse returns s with its runes reversed.\n" +
						"func Reverse(s string) string {\n\tr := []rune(s)\n" +
						"\tfor i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {\n\t\tr[i], r[j] = r[j], r[i]\n\t}\n\treturn string(r)\n}\n\n" +
						"// WordCount counts whitespace-separated words.\nfunc WordCount(s string) int { return len(strings.Fields(s)) }\n",
				},
			}
		case 2:
			action = map[string]any{
				"thought": "add tests",
				"action":  "write_file",
				"args": map[string]any{
					"path": "strutil_test.go",
					"content": "package strutil\n\nimport \"testing\"\n\n" +
						"func TestReverse(t *testing.T) {\n\tif got := Reverse(\"ab\"); got != \"ba\" {\n\t\tt.Fatalf(\"got %q\", got)\n\t}\n}\n\n" +
						"func TestWordCount(t *testing.T) {\n\tif got := WordCount(\"a b\"); got != 2 {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}\n",
				},
			}
		default:
			action = map[string]any{
				"thought": "package and tests are in place",
				"action":  "finish",
				"args":    map[string]any{"summary": "implemented strutil in direct mode"},
			}
		}
		return &llm.Response{
			Content: mustJSON(action),
			Usage:   llm.Usage{PromptTokens: 300, CompletionTokens: 150},
		}, nil
	}}
}

// TestBench_DirectLoopDrivesGoal is the direct-mode counterpart of
// TestBench_GoalRunEndToEnd: the harness must be able to run the persistent
// tool loop against a seeded workspace and account for it. It deliberately
// does not require shell execution — on hosts where run_bash is unavailable the
// acceptance step reports ran=false, and the row must say so rather than claim
// a verified success.
func TestBench_DirectLoopDrivesGoal(t *testing.T) {
	g, ok := goalByName("string-utils")
	if !ok {
		t.Fatal("corpus is missing the string-utils goal")
	}
	bc := benchConfig{
		Model:      "scripted",
		Loop:       loopDirect,
		MaxSteps:   10,
		MaxRetries: 1,
		Timeout:    3 * time.Minute,
		Root:       t.TempDir(),
		Pricing:    cost.DefaultPricing(),
		newClient:  func(agent.Config) llm.Client { return scriptedDirectClient() },
	}

	res := runGoal(context.Background(), g, 1, bc)
	if res.Loop != loopDirect {
		t.Fatalf("loop = %q, want %q", res.Loop, loopDirect)
	}
	if !res.OK {
		t.Fatalf("direct run failed: ok=%v error=%q (workspace %s)", res.OK, res.Error, res.Workdir)
	}
	for _, f := range []string{"strutil.go", "strutil_test.go"} {
		if _, err := os.Stat(filepath.Join(res.Workdir, f)); err != nil {
			t.Fatalf("expected %s in the workspace: %v", f, err)
		}
	}
	if res.LLMCalls < 3 {
		t.Errorf("LLMCalls = %d, want >= 3 (2 writes + finish)", res.LLMCalls)
	}
	if res.TotalTokens <= 0 {
		t.Errorf("token accounting missing: %+v", res)
	}
	// Acceptance must be either genuinely attempted or explicitly refused;
	// silently reporting neither is the failure mode this guards.
	if res.AcceptRan && !res.AcceptOK {
		t.Errorf("acceptance ran and failed: %s", res.AcceptOut)
	}
	if !res.AcceptRan && res.AcceptOut == "" {
		t.Error("skipped acceptance must carry the reason")
	}
}

// TestBench_NormalizeLoop locks -loop parsing: unknown values must be an error,
// not a silent fallback that measures the wrong loop.
func TestBench_NormalizeLoop(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", loopTree},
		{"tree", loopTree},
		{"guided", loopTree},
		{"direct", loopDirect},
		{" DIRECT ", loopDirect},
	} {
		got, err := normalizeLoop(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("normalizeLoop(%q) = %q,%v want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := normalizeLoop("magic"); err == nil {
		t.Fatal("unknown loop must be rejected")
	}
}

// TestBench_CorpusDeclaresAcceptance keeps the direct-mode verdict honest:
// every goal must carry the command that decides it, otherwise a direct run
// could only be scored by the model's own claim.
func TestBench_CorpusDeclaresAcceptance(t *testing.T) {
	for _, g := range Corpus {
		if strings.TrimSpace(g.Accept) == "" {
			t.Errorf("goal %q declares no acceptance command", g.Name)
		}
	}
}

// TestBench_ConfigCLISeedFailsFirst keeps the newest goal honest: a workspace
// with the contract but no implementation must fail, otherwise passing it says
// nothing about the model.
func TestBench_ConfigCLISeedFailsFirst(t *testing.T) {
	g, ok := goalByName("config-cli")
	if !ok {
		t.Fatal("corpus is missing the config-cli goal")
	}
	dir := t.TempDir()
	if err := g.seedWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main_test.go")); err != nil {
		t.Fatalf("main_test.go not seeded: %v", err)
	}
	if out, err := runCmd(dir, "go", "test", "./..."); err == nil {
		t.Fatalf("seeded workspace must fail before the model implements anything:\n%s", out)
	}
}

// TestBench_GuardCatchesEditedTaskFile: the contract test is the verdict, so
// editing it must not count as success.
func TestBench_GuardCatchesEditedTaskFile(t *testing.T) {
	g, ok := goalByName("config-cli")
	if !ok {
		t.Fatal("corpus is missing the config-cli goal")
	}
	dir := t.TempDir()
	if err := g.seedWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	if viol := guardViolation(dir, g); viol != "" {
		t.Fatalf("fresh seed must be intact, got %q", viol)
	}
	path := filepath.Join(dir, "main_test.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The cheapest way to "pass": delete the assertions.
	if err := os.WriteFile(path, append(body, "\n// relaxed\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if viol := guardViolation(dir, g); viol == "" {
		t.Fatal("an edited task file must be reported")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if viol := guardViolation(dir, g); viol == "" || !strings.Contains(viol, "missing") {
		t.Fatalf("a deleted task file must be reported as missing, got %q", viol)
	}
}

// TestBench_GuardPathsAreSeeded stops a typo in Guard from silently guarding
// nothing (which would disable the anti-gaming check for that goal).
func TestBench_GuardPathsAreSeeded(t *testing.T) {
	for _, g := range Corpus {
		for _, rel := range g.Guard {
			if _, ok := g.Seed[rel]; !ok {
				t.Errorf("goal %q guards %q, which it does not seed", g.Name, rel)
			}
		}
	}
}
