//go:build !linux

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRunBash_ExecutionUnavailable pins the contract that a host without
// filesystem isolation reports a typed fatal error instead of a plain
// command failure. Callers use errors.Is to abort the run rather than
// retrying or asking the model to repair code that was never executed.
func TestRunBash_ExecutionUnavailable(t *testing.T) {
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.RunBash(context.Background(), "echo hi", time.Second)
	if res != nil {
		t.Fatalf("expected no result when isolation is unavailable, got %+v", res)
	}
	if !errors.Is(err, ErrExecutionUnavailable) {
		t.Fatalf("expected ErrExecutionUnavailable, got %v", err)
	}
	// The user-facing explanation must survive wrapping: it says why the
	// command was not run and that isolation is the missing piece.
	if !strings.Contains(err.Error(), "isolation") || !strings.Contains(err.Error(), "not executed") {
		t.Fatalf("error should explain the missing isolation, got %q", err)
	}
}
