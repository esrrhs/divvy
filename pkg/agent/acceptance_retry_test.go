package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// An acceptance failure re-queues the integration leaf, but for a long time it
// re-queued it blind: executeLeaf always started with an empty prevError, so the
// leaf re-ran against its own narrower DoD, passed, and the goal failed in
// exactly the same way. This is the regression test for the seed.
func TestExecuteLeaf_SeedsRetryPromptFromAcceptanceFailure(t *testing.T) {
	dod := models.DoD{Commands: []string{"go test ./..."}, TimeoutSec: 10}
	o := buildAcceptanceTree(t, dod)
	o.cfg.MaxRetries = 0

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the requeue happens before the backoff, which ctx cancels
	_ = o.handleAcceptanceFailure(ctx, "$ go test ./...\nexit 1\n--- FAIL: TestLoadConfig\n")

	integrate, _ := o.tree.CloneNode("integrate")
	if !strings.HasPrefix(integrate.ErrorMsg, acceptanceRetryMarker) {
		t.Fatalf("leaf was not marked for seeding: %q", integrate.ErrorMsg)
	}

	// The leaf's own DoD is deliberately trivial. The point is precisely that a
	// leaf which would pass its own check still has to be told about the goal.
	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: []string{"true"}, TimeoutSec: 10}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	client := &llm.ScriptedClient{Handle: func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		cancelRun() // one round trip is all this test needs
		return nil, context.Canceled
	}}
	o.llm = client

	if err := o.executeLeaf(runCtx, integrate); !errors.Is(err, context.Canceled) {
		t.Fatalf("executeLeaf err = %v, want context.Canceled", err)
	}
	if len(client.Requests) == 0 {
		t.Fatal("worker made no request")
	}
	prompt := client.Requests[0].Messages[len(client.Requests[0].Messages)-1].Content
	for _, want := range []string{
		"Previous attempt failed. Fix this error:",
		"--- FAIL: TestLoadConfig",
		"go test ./...",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("retry prompt is missing %q (the acceptance failure never reached the worker)", want)
		}
	}

	// Consumed exactly once: a second retry must not replay a failure the model
	// has already been given, or it re-fixes an already-fixed problem forever.
	again, _ := o.tree.CloneNode("integrate")
	if strings.Contains(again.ErrorMsg, acceptanceRetryMarker) {
		t.Fatalf("retry seed was not consumed: %q", again.ErrorMsg)
	}
}

// The marker is deliberately a prefix, not a substring: ordinary leaf errors
// quote shell output, and a model-written message must never be mistaken for a
// seed carrying engine-authored instructions.
func TestConsumeAcceptanceRetry_IgnoresOrdinaryErrors(t *testing.T) {
	o := buildAcceptanceTree(t, models.DoD{Commands: []string{"true"}})
	for _, msg := range []string{
		"",
		"build failed: exit status 1",
		"interrupted; will retry",
		"mentions " + acceptanceRetryMarker + " in the middle",
	} {
		if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
			n.ErrorMsg = msg
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if seed, ok := o.consumeAcceptanceRetry("integrate"); ok {
			t.Fatalf("%q was mistaken for an acceptance seed (%q)", msg, seed)
		}
		live, _ := o.tree.CloneNode("integrate")
		if live.ErrorMsg != msg {
			t.Fatalf("ErrorMsg was clobbered for %q: %q", msg, live.ErrorMsg)
		}
	}

	if err := o.tree.UpdateNode("integrate", func(n *models.TaskNode) error {
		n.ErrorMsg = acceptanceRetryMarker + " fix the smoke run"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seed, ok := o.consumeAcceptanceRetry("integrate")
	if !ok || seed != "fix the smoke run" {
		t.Fatalf("seed = %q, ok = %v", seed, ok)
	}
	if _, ok := o.consumeAcceptanceRetry("integrate"); ok {
		t.Fatal("a seed must be delivered once, not on every retry")
	}
	if _, ok := o.consumeAcceptanceRetry("no_such_node"); ok {
		t.Fatal("a missing node must not report a seed")
	}
}
