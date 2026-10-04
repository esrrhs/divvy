package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
)

// TestSummarizeEvents checks the post-mortem roll-up of a JSONL event stream,
// including that a malformed line is counted rather than aborting the parse.
func TestSummarizeEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess_test.jsonl")
	lines := []string{
		`{"ts":"2026-01-01T00:00:00Z","kind":"session_start","mode":"run"}`,
		`{"ts":"2026-01-01T00:00:01Z","kind":"llm_call","total_tokens":10}`,
		`{"ts":"2026-01-01T00:00:02Z","kind":"tool_call","ok":false}`,
		`{"ts":"2026-01-01T00:00:03Z","kind":"tool_call","ok":true}`,
		`{"ts":"2026-01-01T00:00:04Z","kind":"verify","ok":false}`,
		`{"ts":"2026-01-01T00:00:05Z","kind":"retry","attempt":1}`,
		`{"ts":"2026-01-01T00:00:06Z","kind":"leaf_stall","run":3}`,
		`{"ts":"2026-01-01T00:00:07Z","kind":"leaf_resplit"}`,
		`{"ts":"2026-01-01T00:00:08Z","kind":"leaf_giveup"}`,
		`{"ts":"2026-01-01T00:00:09Z","kind":"leaf_timeout"}`,
		`{"ts":"2026-01-01T00:00:10Z","kind":"session_end","outcome":"failed","duration_ms":10000}`,
		`this is not json`,
		``,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := SummarizeEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Available {
		t.Fatal("stream should be reported as available")
	}
	if s.LLMCalls != 1 || s.ToolCalls != 2 || s.ToolErrors != 1 {
		t.Fatalf("llm=%d tool=%d toolErr=%d", s.LLMCalls, s.ToolCalls, s.ToolErrors)
	}
	if s.VerifyFail != 1 || s.Retries != 1 || s.Stalls != 1 || s.Resplits != 1 || s.GiveUps != 1 || s.Timeouts != 1 {
		t.Fatalf("verify=%d retry=%d stall=%d resplit=%d giveup=%d timeout=%d",
			s.VerifyFail, s.Retries, s.Stalls, s.Resplits, s.GiveUps, s.Timeouts)
	}
	if s.Outcome != "failed" || s.DurationMS != 10000 {
		t.Fatalf("outcome=%q duration=%d", s.Outcome, s.DurationMS)
	}
	if s.Malformed != 1 {
		t.Fatalf("malformed=%d, want 1", s.Malformed)
	}
	// 11 valid events: start, llm, 2x tool, verify, retry, stall, resplit,
	// giveup, timeout, end. The malformed line is counted separately.
	if s.Total() != 11 {
		t.Fatalf("total events=%d, want 11", s.Total())
	}
	if !s.FirstTS.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first ts=%v", s.FirstTS)
	}
}

// TestSummarizeEventsMissingFile: a session with no event stream must degrade
// to a tree-only report instead of erroring.
func TestSummarizeEventsMissingFile(t *testing.T) {
	s, err := SummarizeEvents(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing stream must not be an error: %v", err)
	}
	if s.Available || s.Total() != 0 {
		t.Fatal("missing stream should report nothing")
	}
}

// TestOrchestrator_ReportCoversSession checks that a report of a finished
// session carries the facts you need after the fact: what was asked, how far
// it got, what it cost.
func TestOrchestrator_ReportCoversSession(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: passingLeafPlan}, nil
			}
			return jsonAction("finish", map[string]string{"summary": "done"}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_report"
	cfg.Goal = "write a file"
	cfg.Stream = false

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	r := o.Report()
	for _, want := range []string{"test_report", "write a file", "COMPLETED", "leaves 1/1", "nodes"} {
		if !strings.Contains(r, want) {
			t.Fatalf("report is missing %q:\n%s", want, r)
		}
	}
	if !strings.Contains(r, "events") {
		t.Fatalf("report should summarise the event stream:\n%s", r)
	}
}

// TestOrchestrator_ReportListsFailures: a failed session's report has to say
// which node failed and why, which is the whole point of reading it later.
func TestOrchestrator_ReportListsFailures(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: failingLeafPlan}, nil
			}
			return jsonAction("finish", map[string]string{"summary": "tried"}), nil
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_report_fail"
	cfg.Goal = "write a library file"
	cfg.Stream = false
	cfg.MaxStall = 2
	cfg.MaxRedecompose = 0
	cfg.RetryMinInterval = 5 * time.Millisecond
	cfg.RetryMaxInterval = 10 * time.Millisecond

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := o.Run(context.Background()); err == nil {
		t.Fatal("run should fail")
	}

	r := o.Report()
	for _, want := range []string{"failures", "FAILED", "stalled"} {
		if !strings.Contains(r, want) {
			t.Fatalf("report is missing %q:\n%s", want, r)
		}
	}
}
