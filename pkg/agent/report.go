package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

// Report renders a human-readable post-mortem of one session: what the goal
// was, how far the tree got, what it cost, and how hard the engine had to
// work (retries, stalls, re-splits, give-ups).
//
// It is deliberately a snapshot of persisted state rather than a live view:
// it is meant to be run after the fact, including on sessions that failed or
// were interrupted, which is exactly when you need it.
func (o *Orchestrator) Report() string {
	var b strings.Builder
	root := o.tree.GetRoot()
	leavesDone, leavesAll, leafPct := o.tree.GetLeafProgress()
	_, nodesAll, _ := o.tree.GetProgress()

	goal := o.cfg.Goal
	if goal == "" && root != nil {
		// Trees saved outside the orchestrator may have an empty Goal; the
		// root title still identifies the session (same fallback -sessions).
		goal = root.Title
	}
	fmt.Fprintf(&b, "session  %s\n", o.tree.ID)
	fmt.Fprintf(&b, "goal     %s\n", reportOneLine(goal))
	fmt.Fprintf(&b, "model    %s\n", o.cfg.Model)
	fmt.Fprintf(&b, "workdir  %s\n", o.sandbox.Root)
	if root != nil {
		state := string(root.State)
		if root.IntegrationVerified {
			state += " (goal acceptance passed)"
		}
		fmt.Fprintf(&b, "state    %s\n", state)
	}
	fmt.Fprintf(&b, "progress leaves %d/%d (%.0f%%)  nodes %d\n", leavesDone, leavesAll, leafPct, nodesAll)

	saved := o.tree.TotalTokenUsage()
	if saved.Calls > 0 {
		line := fmt.Sprintf("tokens   %d (%d calls)", saved.TotalTokens, saved.Calls)
		if c, ok := o.costLabel(llm.Usage{
			PromptTokens:     saved.PromptTokens,
			CompletionTokens: saved.CompletionTokens,
			TotalTokens:      saved.TotalTokens,
		}); ok {
			line += ", est. cost " + c
		}
		fmt.Fprintf(&b, "%s\n", line)
	}

	ev, err := SummarizeEvents(o.EventPath())
	if err != nil {
		fmt.Fprintf(&b, "events   unavailable: %v\n", err)
	}
	if ev.Available {
		fmt.Fprintf(&b, "events   %d  (llm %d, tool %d, verify-fail %d, retry %d, stall %d, resplit %d, giveup %d, timeout %d)\n",
			ev.Total(), ev.LLMCalls, ev.ToolCalls, ev.VerifyFail, ev.Retries,
			ev.Stalls, ev.Resplits, ev.GiveUps, ev.Timeouts)
		if ev.Outcome != "" {
			fmt.Fprintf(&b, "outcome  %s in %s\n", ev.Outcome, time.Duration(ev.DurationMS*int64(time.Millisecond)))
		}
		if ev.Malformed > 0 {
			fmt.Fprintf(&b, "         %d unreadable event line(s)\n", ev.Malformed)
		}
	}

	fmt.Fprintf(&b, "\n%s\n", o.tree.RenderVisualTree())

	nodes := collectNodes(o.tree, root)
	if len(nodes) > 1 {
		b.WriteString("\nnodes\n")
		fmt.Fprintf(&b, "  %-16s %-11s %5s %9s %8s  %s\n", "ID", "STATE", "RETRY", "TOKENS", "COST", "TITLE")
		for _, n := range nodes {
			cost := "-"
			if n.TokenUsage.TotalTokens > 0 {
				if c, ok := o.costLabel(llm.Usage{
					PromptTokens:     n.TokenUsage.PromptTokens,
					CompletionTokens: n.TokenUsage.CompletionTokens,
					TotalTokens:      n.TokenUsage.TotalTokens,
				}); ok {
					cost = c
				}
			}
			fmt.Fprintf(&b, "  %-16s %-11s %5d %9d %8s  %s\n",
				n.ID, n.State, n.RetryCount, n.TokenUsage.TotalTokens, cost, reportOneLine(n.Title))
		}
	}

	var failed []*models.TaskNode
	for _, n := range nodes {
		if n.State == models.TaskStateFailed || n.ErrorMsg != "" {
			failed = append(failed, n)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\nfailures\n")
		for _, n := range failed {
			fmt.Fprintf(&b, "  %s [%s] %s\n", n.ID, n.State, reportOneLine(n.Title))
			if n.ErrorMsg != "" {
				fmt.Fprintf(&b, "    %s\n", truncate(strings.ReplaceAll(n.ErrorMsg, "\n", "\n    "), 600))
			}
		}
	}
	return b.String()
}

// collectNodes flattens the tree depth-first so the report lists nodes in the
// same order as the rendered tree.
func collectNodes(tree treeLister, root *models.TaskNode) []*models.TaskNode {
	if root == nil {
		return nil
	}
	out := []*models.TaskNode{root}
	for _, id := range root.ChildrenIDs {
		if child, ok := tree.GetNode(id); ok {
			out = append(out, collectNodes(tree, child)...)
		}
	}
	return out
}

// treeLister is the read-only slice of TaskTree the report needs, so the
// walk can be tested without a full tree.
type treeLister interface {
	GetNode(id string) (*models.TaskNode, bool)
}

// reportOneLine collapses a title/goal to a single line of bounded width.
func reportOneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
