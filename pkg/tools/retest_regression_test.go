package tools

// Regression copy of the independent retest (forward safety assertions).
// Source: /workspace/divvy-audit/retest/tools_retest.go — that tree is not modified.
import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegressionRetestMergeCompensationPreservesExternalEdits(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a"), []byte("base-a"), 0644)
	os.WriteFile(filepath.Join(src, "z"), []byte("base-z"), 0644)
	m, e := NewMirror(src)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	os.WriteFile(filepath.Join(m.Dir, "a"), []byte("agent-a"), 0644)
	os.WriteFile(filepath.Join(m.Dir, "z"), []byte("agent-z"), 0644)
	mergeWriteHook = func(rel string) {
		if rel == "z" {
			os.WriteFile(filepath.Join(src, "a"), []byte("external-new-a"), 0644)
			os.WriteFile(filepath.Join(src, "z"), []byte("external-new-z"), 0644)
		}
	}
	defer func() { mergeWriteHook = nil }()
	if _, _, e = m.MergeBack(); e == nil {
		t.Fatal("want conflict")
	}
	v, _ := os.ReadFile(filepath.Join(src, "a"))
	if string(v) != "external-new-a" {
		t.Fatalf("compensating rollback overwrote later external edit to already-written a: got %q", v)
	}
}
