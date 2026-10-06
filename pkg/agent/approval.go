package agent

import (
	"context"

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
}

// LeafApprovalHook blocks until the reviewer decides (or ctx is cancelled).
// Manual approval mode is inert until a hook is installed.
type LeafApprovalHook func(ctx context.Context, req LeafApprovalRequest) (LeafApprovalDecision, error)
