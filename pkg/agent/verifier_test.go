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

	// A prose expected_output ("Build succeeds") on a quiet successful build
	// must be tolerated: exit codes are the verdict, not an unmatchable
	// sentence. This used to force the leaf into pointless re-splitting.
	node.DoD = models.DoD{
		Commands:       []string{"true"},
		ExpectedOutput: "Build succeeds",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); !vr.OK {
		t.Fatalf("prose expected_output on quiet success should pass: %s", vr.Output)
	}

	// A concrete token expected by a command that prints nothing still fails.
	node.DoD = models.DoD{
		Commands:       []string{"true"},
		ExpectedOutput: "ok",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("literal expected_output that is absent must fail")
	}

	// Prose expected_output is enforced when the commands actually printed
	// something (real mismatch must not be silently ignored).
	node.DoD = models.DoD{
		Commands:       []string{"echo real-output"},
		ExpectedOutput: "build returns success",
		TimeoutSec:     10,
	}
	if vr := o.verify(context.Background(), sb, node); vr.OK {
		t.Fatal("mismatch with non-empty output should fail")
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
