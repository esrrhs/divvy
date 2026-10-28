package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// The regression this file guards: a workspace whose tree is addressable two
// ways. A leaf works in a copy mirror, which drops symlinks, so the alias and
// the real directory become different files at merge time. The run cannot see
// that from inside a leaf, so it has to be reported before leaves start.

func TestLayoutWarnings_ReportsSymlinkAlias(t *testing.T) {
	o := structureFixture(t, "重构内部包", []string{"go test ./..."},
		map[string][]string{"core": {"internal/core/core.go"}})

	if ws := o.layoutWarnings(); len(ws) != 0 {
		t.Fatalf("clean workspace reported %v", ws)
	}

	if err := os.MkdirAll(filepath.Join(o.sandbox.Root, "internal", "core"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.sandbox.Root, "internal", "core", "core.go"), []byte("package core\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("internal", "core"), filepath.Join(o.sandbox.Root, "core")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	ws := o.layoutWarnings()
	if len(ws) != 1 {
		t.Fatalf("warnings = %v, want 1", ws)
	}
	for _, want := range []string{"core", "symlink", "internal/core"} {
		if !strings.Contains(ws[0], want) {
			t.Errorf("warning %q does not mention %q", ws[0], want)
		}
	}
}

func TestLayoutWarnings_SurvivesUnreadableRoot(t *testing.T) {
	o := structureFixture(t, "重构内部包", []string{"go test ./..."}, nil)
	// A missing workdir must degrade to "no warnings", never to a failed run:
	// the layout check is a diagnostic, not a gate.
	o.sandbox.Root = filepath.Join(o.sandbox.Root, "does-not-exist")
	if ws := o.layoutWarnings(); len(ws) != 0 {
		t.Fatalf("warnings = %v", ws)
	}
}

// TestRunPlan_StrictFailsOnLayoutIssue pins the -strict wiring: a workspace
// that needs fixing must fail a CI plan run even when the plan itself is fine.
func TestRunPlan_StrictFailsOnLayoutIssue(t *testing.T) {
	newPlanned := func(t *testing.T) *Orchestrator {
		o := structureFixture(t, "重构内部包", []string{"go test ./..."},
			map[string][]string{"core": {"internal/core/core.go"}})
		o.cfg.Strict = true
		// structureFixture leaves leaf DoD empty; give it one so the plan check
		// is genuinely clean and only the layout check can fail the run.
		for _, leaf := range o.tree.Leaves() {
			if err := o.tree.UpdateNode(leaf.ID, func(n *models.TaskNode) error {
				n.DoD = models.DoD{Commands: []string{"go test ./..."}, TimeoutSec: 60}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if ws := o.layoutWarnings(); len(ws) != 0 {
			t.Fatalf("fixture is not clean: %v", ws)
		}
		if ws := o.planWarnings(); len(ws) != 0 {
			t.Fatalf("fixture plan is not clean: %v", ws)
		}
		return o
	}

	t.Run("clean workspace passes", func(t *testing.T) {
		o := newPlanned(t)
		if err := o.RunPlan(t.Context()); err != nil {
			t.Fatalf("RunPlan on a clean workspace: %v", err)
		}
	})

	t.Run("symlink alias fails", func(t *testing.T) {
		o := newPlanned(t)
		if err := os.MkdirAll(filepath.Join(o.sandbox.Root, "internal", "core"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("internal", "core"), filepath.Join(o.sandbox.Root, "core")); err != nil {
			t.Skipf("symlinks unsupported here: %v", err)
		}
		err := o.RunPlan(t.Context())
		if err == nil {
			t.Fatal("RunPlan under -strict accepted a workspace with a symlink alias")
		}
		if !strings.Contains(err.Error(), "warning") {
			t.Errorf("error = %v", err)
		}
	})
}

// TestPublishLeaf_PropagatesLayoutError keeps the type intact through the merge
// call site: the run loop decides "retry or fail" with errors.As, so a wrapped
// or generic error here would send an unwinnable case back into retries.
func TestPublishLeaf_PropagatesLayoutError(t *testing.T) {
	o := structureFixture(t, "重构内部包", []string{"go test ./..."},
		map[string][]string{"core": {"internal/core/core.go"}})
	root := o.sandbox.Root
	if err := os.MkdirAll(filepath.Join(root, "internal", "core"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "core", "core_test.go"), []byte("package core // pre-existing\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("internal", "core"), filepath.Join(root, "core")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	m, err := tools.NewMirror(root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.MkdirAll(filepath.Join(m.Dir, "core"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir, "core", "core_test.go"), []byte("package core // from leaf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	leaf, ok := o.tree.CloneNode("core")
	if !ok {
		t.Fatal("fixture leaf missing")
	}

	_, err = o.publishLeaf(m, leaf, "")
	var layout *tools.MirrorLayoutError
	if !errors.As(err, &layout) {
		t.Fatalf("publishLeaf error = %v, want *MirrorLayoutError", err)
	}
}
