package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/esrrhs/divvy/pkg/tools"
)

// LeafApprovalRequest is presented to a human reviewer after a leaf finished
// and verified, but before any of its changes are merged back.
type LeafApprovalRequest struct {
	NodeID  string
	Title   string
	Changes []tools.FileChange
}

// LeafApprovalDecision is the reviewer's verdict: approval proceeds to
// MergeBack; rejection discards the leaf's mirror, re-snapshots the
// workspace and re-runs the leaf with the comment fed back as prior error.
type LeafApprovalDecision struct {
	Approved bool
	Comment  string
	// Patches replace named files in the isolated mirror after approval and
	// before MergeBack (used for hunk-level accept/reject). Only paths that
	// already appear in the review set are written. Deletes removes a path
	// from the mirror (e.g. a newly added file whose hunks were all rejected).
	Patches map[string]string
	Deletes []string
}

// LeafApprovalHook blocks until the reviewer decides (or ctx is cancelled).
// Manual approval mode is inert until a hook is installed.
type LeafApprovalHook func(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error)

func applyLeafApprovalPatches(mirror *tools.Mirror, allowed []tools.FileChange, d LeafApprovalDecision) error {
	if mirror == nil {
		return nil
	}
	if len(d.Patches) == 0 && len(d.Deletes) == 0 {
		return nil
	}
	allow := make(map[string]bool, len(allowed))
	for _, c := range allowed {
		allow[filepath.ToSlash(c.Path)] = true
	}
	root, err := filepath.EvalSymlinks(mirror.Dir)
	if err != nil {
		root = mirror.Dir
	}
	write := func(rel, content string) error {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || !allow[rel] {
			return fmt.Errorf("patch path %q is not in this leaf's review set", rel)
		}
		abs := filepath.Join(mirror.Dir, filepath.FromSlash(rel))
		real, err := filepath.Abs(abs)
		if err != nil {
			return err
		}
		if resolved, e := filepath.EvalSymlinks(filepath.Dir(real)); e == nil {
			if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
				return fmt.Errorf("patch path %q escapes the leaf workspace", rel)
			}
		}
		if err := os.MkdirAll(filepath.Dir(real), 0755); err != nil {
			return err
		}
		return os.WriteFile(real, []byte(content), 0644)
	}
	for rel, body := range d.Patches {
		if err := write(rel, body); err != nil {
			return err
		}
	}
	for _, rel := range d.Deletes {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || !allow[rel] {
			return fmt.Errorf("delete path %q is not in this leaf's review set", rel)
		}
		abs := filepath.Join(mirror.Dir, filepath.FromSlash(rel))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
