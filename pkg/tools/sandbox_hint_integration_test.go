//go:build linux || darwin

package tools

import (
	"context"
	"strings"
	"testing"
)

// The end-to-end half of sandbox_hint_test.go: a real refusal coming out of a
// real sandbox must reach the model with the hint attached. Without this, the
// helper can be correct and still be wired to nothing.
//
// /etc is the target because neither backend can ever allow it (bwrap ro-binds
// the root filesystem; seatbelt allows only system prefixes read-only), while
// the workspace stays writable in the same command — so the test cannot pass
// because everything was denied.
func TestRunBash_ExplainsSandboxWriteRefusal(t *testing.T) {
	root := t.TempDir()
	if ok, reason := ProbeShellIsolation(root); !ok {
		t.Skipf("no execution backend here: %s", reason)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyRuntimePolicy(sb)
	if reason, off := sb.ToolDisabled(ToolRunBash); off {
		t.Skipf("run_bash disabled: %s", reason)
	}

	out, err := sb.Call(context.Background(), ToolRunBash, map[string]any{
		"command": "printf ok > inside.txt && printf x > /etc/divvy-hint-probe",
	})
	if err != nil {
		t.Fatalf("run_bash: %v", err)
	}
	if !strings.Contains(out, "exit_code: 1") {
		t.Fatalf("the write must fail, got:\n%s", out)
	}
	if !strings.Contains(out, sandboxWriteHint) {
		t.Fatalf("refused write reached the model without an explanation:\n%s", out)
	}

	// The workspace half still worked: a hint that fires on a command where
	// nothing was denied would be noise.
	okOut, err := sb.Call(context.Background(), ToolRunBash, map[string]any{"command": "ls inside.txt"})
	if err != nil {
		t.Fatalf("run_bash: %v", err)
	}
	if strings.Contains(okOut, sandboxWriteHint) {
		t.Fatalf("hint appended to a successful command:\n%s", okOut)
	}
}
