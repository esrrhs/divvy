package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// TestDirect_RepeatedVerifyFailureDemandsObservation covers the loop shape the
// repeated-call guard misses: the model keeps editing and re-running the same
// failing verification command. The calls differ, so nothing looks repeated,
// but the failure output does not change. After the second identical failure
// the model must be told to observe before editing again.
func TestDirect_RepeatedVerifyFailureDemandsObservation(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := mgrCfg(work, data, "sess_repeatfail_"+t.Name())
	tree := engine.NewTaskTree("t-repeat-fail", "root", "make the test pass")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)

	act := taggedAction{Name: tools.ToolRunBash, Args: map[string]any{"command": "go test ./..."}}
	failOut := "exit_code: 1\n--- FAIL: TestAdd (0.00s)\n    add_test.go:7: Add(2,3)=-1"

	if msg, nag := d.trackRepeatedFailure(act, failOut, nil); nag || msg != "" {
		t.Fatalf("first failure must not nag, got %q", msg)
	}
	msg, nag := d.trackRepeatedFailure(act, failOut, nil)
	if !nag {
		t.Fatal("second identical failure should demand a strategy change")
	}
	if !strings.Contains(msg, "go test ./...") || !strings.Contains(msg, "failed the same way 2 times") {
		t.Fatalf("escalation should name the command and repetition count:\n%s", msg)
	}
	if !strings.Contains(msg, "Observe it") {
		t.Fatalf("escalation should ask for an observation, not another guess:\n%s", msg)
	}

	// A passing run ends the failure streak: the next failure is a new
	// problem and must not inherit the previous count.
	if _, nag := d.trackRepeatedFailure(act, "exit_code: 0\nok  \taddtest", nil); nag {
		t.Fatal("a passing run must reset the failure streak")
	}
	if d.failKey != "" || d.failCount != 0 {
		t.Fatalf("successful run should clear the streak, got key=%q count=%d", d.failKey, d.failCount)
	}

	// A different failing command is a different problem, not a repeat.
	if _, nag := d.trackRepeatedFailure(act, failOut, nil); nag {
		t.Fatal("first failure after reset must not nag")
	}
	other := taggedAction{Name: tools.ToolRunBash, Args: map[string]any{"command": "go vet ./..."}}
	if msg, nag := d.trackRepeatedFailure(other, "exit_code: 1\nvet: undefined: Foo", nil); nag || msg != "" {
		t.Fatalf("a different command must not be folded into the previous streak: %q", msg)
	}

	// Nags are capped per turn so the intervention cannot become the loop.
	d = NewDirect(o, nil)
	for i := 0; i < directFailureLimit; i++ {
		d.trackRepeatedFailure(act, failOut, nil)
	}
	for i := 0; i < directFailureLimit*2; i++ {
		d.trackRepeatedFailure(act, failOut, nil)
	}
	if d.failNags < directFailureNags {
		t.Fatalf("expected the cap to be reached, nags=%d", d.failNags)
	}
	if _, nag := d.trackRepeatedFailure(act, failOut, nil); nag {
		t.Fatalf("nags must stop after %d per turn", directFailureNags)
	}
}

// TestDirect_RepeatedVerifyFailureReachesTranscript wires the guard through
// applyActions: two failing run_bash calls with identical output must leave the
// strategy prompt in the transcript the next model step sees.
func TestDirect_RepeatedVerifyFailureReachesTranscript(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := mgrCfg(work, data, "sess_repeatfail_transcript_"+t.Name())
	tree := engine.NewTaskTree("t-repeat-fail-transcript", "root", "goal")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	d.st = &directState{Version: 1, Messages: []llm.Message{{Role: llm.RoleUser, Content: "make it pass"}}}

	act := taggedAction{ID: "call-1", Name: tools.ToolRunBash, Args: map[string]any{"command": "exit 1", "timeout_sec": 10}}
	for i := 0; i < directFailureLimit; i++ {
		finished, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{act})
		if err != nil || finished {
			t.Fatalf("applyActions(%d): finished=%v err=%v", i, finished, err)
		}
	}
	joined := ""
	for _, m := range d.st.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "failed the same way 2 times") {
		t.Fatalf("transcript should contain the escalation prompt, got:\n%s", joined)
	}
}

// TestDirect_NonVerifyFailureDoesNotEscalate keeps the new guard scoped to
// verification commands: a failed read or edit gets the normal tool-error
// feedback, not a diagnosis lecture.
func TestDirect_NonVerifyFailureDoesNotEscalate(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := mgrCfg(work, data, "sess_repeatfail_"+t.Name())
	tree := engine.NewTaskTree("t-repeat-fail-edit", "root", "goal")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	act := taggedAction{Name: tools.ToolReplaceLines, Args: map[string]any{"path": "a.go"}}
	for i := 0; i < 5; i++ {
		if msg, nag := d.trackRepeatedFailure(act, "ERROR: old_string not found", nil); nag || msg != "" {
			t.Fatalf("non-verify failure must not trigger the strategy prompt: %q", msg)
		}
	}
}
