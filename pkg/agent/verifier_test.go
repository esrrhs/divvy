package agent

import (
	"context"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/models"
	"github.com/esrrhs/go_llm_engine/pkg/tools"
)

func TestVerifyExpectedOutputAcrossCommands(t *testing.T) {
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{log: SilentLogger()}

	node := &models.TaskNode{
		DoD: models.DoD{
			// First command emits nothing; only the second one contains
			// the expected text. Under the old per-command rule the quiet
			// first command made verification impossible.
			Commands:       []string{"true", "echo marker-ok"},
			ExpectedOutput: "marker-ok",
			TimeoutSec:     10,
		},
	}
	vr := o.verify(context.Background(), sb, node)
	if !vr.OK {
		t.Fatalf("expected pass from combined output, got: %s", vr.Output)
	}

	// Missing expected text after all commands passed → fail.
	node.DoD.ExpectedOutput = "nope"
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("expected failure for absent expected_output")
	}

	// Failing command still fails regardless of expected text.
	node.DoD = models.DoD{
		Commands:       []string{"echo marker-ok", "false"},
		ExpectedOutput: "marker-ok",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("expected failure from non-zero command")
	}
}
