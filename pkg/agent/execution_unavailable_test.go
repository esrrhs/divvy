//go:build !linux

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// TestVerify_ExecutionUnavailableAbortsRun covers the macOS/non-Linux case
// where run_bash has no filesystem isolation. Verification must surface the
// typed environment error, and aborting must mark the root FAILED instead of
// spending the retry budget trying to "repair" a command that never ran.
func TestVerify_ExecutionUnavailableAbortsRun(t *testing.T) {
	if ok, _ := tools.ProbeShellIsolation(t.TempDir()); ok {
		t.Skip("this host has an execution backend; the unavailable path only exists without one")
	}
	work, data := t.TempDir(), t.TempDir()
	cfg := mgrCfg(work, data, "sess_unavailable_"+t.Name())
	tree := engine.NewTaskTree("t-unavailable", "root", "prove commands cannot run")
	if err := tree.UpdateNode(tree.RootID, func(n *models.TaskNode) error {
		n.DoD = models.DoD{Commands: []string{"echo hi"}, TimeoutSec: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)

	root, ok := tree.CloneNode(tree.RootID)
	if !ok {
		t.Fatal("root missing")
	}
	vr := o.verify(context.Background(), o.sandbox, root)
	if vr.OK || !vr.Unavailable {
		t.Fatalf("expected an unavailable verification, got %+v", vr)
	}
	if err := vr.UnavailableErr(); !errors.Is(err, tools.ErrExecutionUnavailable) {
		t.Fatalf("UnavailableErr should wrap the typed error, got %v", err)
	}

	if err := o.abortExecutionUnavailable(tree.RootID, vr); !errors.Is(err, tools.ErrExecutionUnavailable) {
		t.Fatalf("abort should return the typed error, got %v", err)
	}
	got, ok := tree.CloneNode(tree.RootID)
	if !ok {
		t.Fatal("root missing after abort")
	}
	if got.State != models.TaskStateFailed {
		t.Fatalf("root state = %s, want FAILED", got.State)
	}
	if got.ErrorMsg == "" {
		t.Fatal("root ErrorMsg should explain why the run stopped")
	}
}
