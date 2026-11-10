package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditMergeOverwritesExternalEditAfterPreflight(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0755)
	a, z := filepath.Join(src, "a"), filepath.Join(src, "z")
	os.WriteFile(a, []byte("base"), 0644)
	os.WriteFile(z, []byte("base"), 0644)
	m, err := NewMirrorAt(src, filepath.Join(root, "mirror"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.WriteFile(filepath.Join(m.Dir, "a"), []byte("agent-a"), 0644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(m.Dir, "z"), []byte("agent"), 0644)
	// Deterministic mid-merge: after preflight, mutate z just before it is written.
	// a is already copied in this hook when rel=="z" (writes are sorted).
	mergeWriteHook = func(rel string) {
		if rel == "z" {
			_ = os.WriteFile(z, []byte("external-user-edit"), 0644)
		}
	}
	t.Cleanup(func() { mergeWriteHook = nil })
	_, _, err = m.MergeBack()
	v, rerr := os.ReadFile(z)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(v) == "agent" {
		t.Fatal("MergeBack overwrote z after an external edit that landed mid-merge")
	}
	if string(v) != "external-user-edit" {
		t.Fatalf("z should keep the external edit, got %q (err=%v)", v, err)
	}
	if err == nil {
		t.Fatal("MergeBack should report a conflict when z changed mid-merge")
	}
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected MergeConflictError, got %v", err)
	}
	// Already-written a should be restored from the pre-merge backup.
	if got, _ := os.ReadFile(a); string(got) != "base" {
		t.Fatalf("compensating restore of a failed: %q", got)
	}
}

func TestRunBashDoesNotWriteOutsideWorkspace(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	dest := filepath.Join(outside, "marker")
	sb, err := NewSandbox(work)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.RunBash(context.Background(), "printf escaped > "+dest+"; echo rc=$?", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("run_bash wrote outside the workspace; result=%+v", res)
	}
}
