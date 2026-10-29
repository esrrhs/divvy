package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

type decomposeResult struct {
	IsAtomic bool                `json:"is_atomic"`
	Reason   string              `json:"reason"`
	Contract models.ContractSpec `json:"contract"`
	DoD      models.DoD          `json:"dod"`
	Subtasks []decomposeChild    `json:"subtasks"`
}

type decomposeChild struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Type        string              `json:"type"`
	Contract    models.ContractSpec `json:"contract"`
	DoD         models.DoD          `json:"dod"`
}

func (o *Orchestrator) decompose(ctx context.Context, node *models.TaskNode) error {
	// Never expand a node that already has children. applyDecompose rolls back
	// by calling ResetChildren, which deletes the subtree — correct while the
	// children it just created are still PENDING, catastrophic if they have
	// already run. The scheduler only hands out childless compounds today, so
	// this is a guard against a future caller, not a live path.
	if cur, ok := o.tree.CloneNode(node.ID); ok && len(cur.ChildrenIDs) > 0 {
		return fmt.Errorf("node %s already has %d child(ren); refusing to re-decompose and discard completed work",
			node.ID, len(cur.ChildrenIDs))
	}
	planRepair := strings.Contains(node.ErrorMsg, planRepairMarker)
	if node.Depth >= o.cfg.MaxDepth && !planRepair {
		o.log.Warnf("max depth %d reached, forcing %s to leaf", o.cfg.MaxDepth, node.ID)
		return o.forceLeaf(node)
	}
	// At MaxDepth a plan-repair must stay atomic: we rewrite contract/DoD,
	// we do not grow the tree. The prompt below forces is_atomic=true.
	forceAtomic := planRepair && node.Depth >= o.cfg.MaxDepth
	if forceAtomic {
		o.log.Warnf("max depth %d: atomic plan-repair for %s", o.cfg.MaxDepth, node.ID)
	}

	if err := o.sched.UpdateNodeState(node.ID, models.TaskStateDecomposing, ""); err != nil {
		return err
	}

	user := o.decomposeUserPrompt(node)
	if forceAtomic {
		user += "\n\nHARD REQUIREMENT: set is_atomic=true with an empty subtasks array. " +
			"Rewrite contract+dod for THIS task only. Fix any unsatisfiable expected_output."
	}
	base := []llm.Message{
		{Role: llm.RoleSystem, Content: decomposerSystem},
		{Role: llm.RoleUser, Content: user},
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		var planErr *planIncompleteError
		structureIssue := errors.As(lastErr, &planErr)

		// A structural replan is a conversation with the planner, not a
		// transient failure: retrying immediately with a pointed message is
		// the whole point, so no backoff.
		if lastErr != nil && !structureIssue {
			delay := RetryDelay(attempt-1, o.cfg.retryMin(), o.cfg.retryMax())
			o.log.Warnf("decompose %s failed: %v; retry in %s", node.ID, lastErr, delay)
			_ = o.checkpoint()
			if err := waitBackoff(ctx, delay); err != nil {
				return err
			}
		}

		messages := append([]llm.Message{}, base...)
		switch {
		case structureIssue:
			messages = append(messages, llm.Message{
				Role: llm.RoleUser,
				Content: "Your plan is structurally incomplete:\n" + planErr.Error() +
					"\n\nRe-issue the WHOLE plan with this fixed. Every path and command in the " +
					"goal-level DoD must be produced by some subtask, and a runnable goal needs a " +
					"subtask that creates its entry point. Return ONLY corrected JSON.",
			})
		case lastErr != nil:
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: "Your previous JSON was invalid:\n" + lastErr.Error() + "\n\nReturn ONLY corrected JSON.",
			})
		}

		o.log.Actionf("decompose %s (attempt %d)", node.ID, attempt)
		resp, err := o.chat(ctx, "decompose", node.ID, llm.Request{
			Model:       o.cfg.Model,
			Messages:    messages,
			Temperature: o.cfg.Temperature,
			MaxTokens:   o.cfg.MaxTokens,
			Stream:      o.cfg.Stream,
		})
		if err != nil {
			lastErr = err
		} else {
			o.log.Debugf("decompose raw: %s", truncate(resp.Content, 500))
			var parsed decomposeResult
			if err := llm.UnmarshalFlexible(resp.Content, &parsed); err != nil {
				lastErr = err
			} else {
				if forceAtomic {
					parsed.IsAtomic = true
					parsed.Subtasks = nil
				}
				if err := o.applyDecompose(node, parsed); err != nil {
					lastErr = err
				} else {
					return nil
				}
			}
		}

		if o.cfg.DecomposeTries > 0 && attempt >= o.cfg.DecomposeTries && !structureIssue {
			o.log.Warnf("decompose failed for %s: %v; forcing leaf", node.ID, lastErr)
			return o.forceLeaf(node)
		}
	}
}

func (o *Orchestrator) decomposeUserPrompt(node *models.TaskNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n", o.sandbox.Root)
	if p := o.projectType(); p != tools.ProjectGeneric {
		fmt.Fprintf(&b, "Detected toolchain: %s — write DoD verification commands for THIS stack (not Go).\n", p)
	}
	fmt.Fprintf(&b, "Max subtasks: %d\nMax remaining depth: %d\n\n", o.cfg.MaxSubtasks, o.cfg.MaxDepth-node.Depth)
	fmt.Fprintf(&b, "Task ID: %s\nTitle: %s\nDescription:\n%s\n\n", node.ID, node.Title, node.Description)
	b.WriteString("Existing contract:\n")
	b.WriteString(formatContract(node.Contract))
	b.WriteString("\nExisting DoD:\n")
	b.WriteString(formatDoD(node.DoD))
	if node.ErrorMsg != "" {
		switch {
		case strings.Contains(node.ErrorMsg, planStructureMarker):
			fmt.Fprintf(&b, "\nPrevious plan was structurally incomplete — fix the SHAPE of the "+
				"plan before splitting further (a subtask must produce the entry point and every "+
				"path the goal-level DoD runs):\n%s\n",
				truncate(node.ErrorMsg, 2500))
		case strings.Contains(node.ErrorMsg, planRepairMarker):
			fmt.Fprintf(&b, "\nPrevious failure — DIAGNOSE THE PLAN FIRST (DoD/contract may be wrong; "+
				"prefer is_atomic=true and fix expected_output/commands before splitting further):\n%s\n",
				truncate(node.ErrorMsg, 2500))
		default:
			fmt.Fprintf(&b, "\nPrevious failure (you MUST split smaller and address this):\n%s\n", truncate(node.ErrorMsg, 2500))
		}
	}
	if node.ParentID != "" {
		if p, ok := o.tree.CloneNode(node.ParentID); ok {
			fmt.Fprintf(&b, "\nParent: %s — %s\nParent contract:\n%s\n", p.ID, p.Title, formatContract(p.Contract))
		}
	}
	b.WriteString("\nWorkspace snapshot:\n")
	b.WriteString(o.sandbox.Snapshot())
	return b.String()
}

func (o *Orchestrator) applyDecompose(node *models.TaskNode, parsed decomposeResult) error {
	if parsed.IsAtomic || len(parsed.Subtasks) == 0 {
		if len(parsed.Subtasks) == 1 {
			parsed.Contract = mergeContract(parsed.Contract, parsed.Subtasks[0].Contract)
			parsed.DoD = mergeDoD(parsed.DoD, parsed.Subtasks[0].DoD)
		}
		return o.convertToLeaf(node.ID, parsed.Contract, parsed.DoD)
	}

	if len(parsed.Subtasks) == 1 {
		return fmt.Errorf("non-atomic split must have at least 2 subtasks")
	}
	if len(parsed.Subtasks) > o.cfg.MaxSubtasks {
		parsed.Subtasks = parsed.Subtasks[:o.cfg.MaxSubtasks]
	}

	// The top-level DoD is the goal-level acceptance run after every child
	// completes. Persist it on the compound node; give it sane defaults when
	// the model omitted commands. An operator-supplied acceptance wins: it is
	// the contract the run is judged by, not a suggestion the planner may
	// rewrite.
	goalDoD := parsed.DoD
	ensureGoalDoD(&goalDoD, o.projectType())
	if len(o.cfg.DoDCommands) > 0 && node.ID == o.tree.RootID {
		goalDoD.Commands = append([]string(nil), o.cfg.DoDCommands...)
	}
	if err := o.tree.UpdateNode(node.ID, func(n *models.TaskNode) error {
		n.DoD = goalDoD
		return nil
	}); err != nil {
		return err
	}

	children, err := normalizeChildren(parsed.Subtasks, o.tree, node)
	if err != nil {
		return err
	}

	for i := range children {
		ch := children[i]
		nodeType := models.NodeTypeLeaf
		if strings.EqualFold(ch.Type, string(models.NodeTypeCompound)) && node.Depth+1 < o.cfg.MaxDepth {
			nodeType = models.NodeTypeCompound
		}
		created, err := o.tree.AddChild(node.ID, ch.ID, ch.Title, ch.Description, nodeType)
		if err != nil {
			return err
		}
		dod := ch.DoD
		if nodeType == models.NodeTypeLeaf {
			ensureDoD(&dod, ch.Contract.Outputs, o.projectType())
		}
		err = o.tree.UpdateNode(created.ID, func(n *models.TaskNode) error {
			n.Contract = ch.Contract
			n.DoD = dod
			n.MaxRetries = o.cfg.MaxRetries
			return nil
		})
		if err != nil {
			return err
		}
		kind := "leaf"
		if nodeType == models.NodeTypeCompound {
			kind = "compound"
		}
		o.log.Actionf("  + %s %s: %s", kind, ch.ID, ch.Title)
	}

	if err := o.sched.ValidateDependencies(); err != nil {
		_ = o.tree.ResetChildren(node.ID)
		return err
	}

	// A runnable goal whose plan cannot assemble is unrecoverable later: the
	// goal-level acceptance has no integration leaf to route its failure to,
	// so the root goes straight to FAILED and no retry can repair it. Catch it
	// here, while every leaf is still unexecuted and a replan is still cheap.
	if node.ID == o.tree.RootID {
		if blockers := o.planBlockers(); len(blockers) > 0 {
			// node is a snapshot taken before decomposition; read the live
			// counter or the replan budget never advances and this loops.
			cur, _ := o.tree.CloneNode(node.ID)
			if cur != nil && cur.DecomposeCount < o.cfg.MaxRedecompose {
				o.log.Warnf("plan is structurally incomplete, replanning %s:", node.ID)
				for _, b := range blockers {
					o.log.Warnf("  - %s", b)
				}
				_ = o.tree.UpdateNode(node.ID, func(n *models.TaskNode) error {
					n.DecomposeCount++
					n.ErrorMsg = planStructureMarker + strings.Join(blockers, "\n")
					return nil
				})
				_ = o.tree.ResetChildren(node.ID)
				return &planIncompleteError{blockers: blockers}
			}
			// Out of replan budget: stop negotiating with the planner and add
			// the missing piece ourselves, so the goal stays reachable.
			if _, err := o.ensureIntegrationLeaf(strings.Join(blockers, "; ")); err != nil {
				return fmt.Errorf("plan incomplete and cannot heal: %w", err)
			}
		}
	}

	return o.sched.UpdateNodeState(node.ID, models.TaskStateRunning, "")
}

func (o *Orchestrator) convertToLeaf(id string, contract models.ContractSpec, dod models.DoD) error {
	ensureDoD(&dod, contract.Outputs, o.projectType())
	if err := o.tree.ConvertToLeaf(id, contract, dod); err != nil {
		return err
	}
	o.log.Okf("%s is atomic → leaf", id)
	o.sched.RefreshAncestors(id)
	return nil
}

func (o *Orchestrator) forceLeaf(node *models.TaskNode) error {
	return o.convertToLeaf(node.ID, node.Contract, node.DoD)
}

// projectType identifies the workspace toolchain so defaults are generated
// in the right language.
func (o *Orchestrator) projectType() tools.ProjectType {
	return tools.DetectProject(o.sandbox.Root)
}

func normalizeChildren(raw []decomposeChild, tree *engine.TaskTree, parent *models.TaskNode) ([]decomposeChild, error) {
	out := make([]decomposeChild, 0, len(raw))
	idMap := map[string]string{} // original -> unique

	taken := func(id string) bool {
		if id == parent.ID || id == "root" {
			return true
		}
		if tree.HasNode(id) {
			return true
		}
		for _, c := range out {
			if c.ID == id {
				return true
			}
		}
		return false
	}

	for _, ch := range raw {
		title := strings.TrimSpace(ch.Title)
		desc := strings.TrimSpace(ch.Description)
		if title == "" {
			title = "untitled"
		}
		if desc == "" {
			desc = title
		}
		orig := strings.TrimSpace(ch.ID)
		base := sanitizeID(orig)
		if base == "task" {
			base = sanitizeID(title)
		}
		id := uniqueID(base, taken)
		idMap[orig] = id
		if orig != "" {
			idMap[strings.ToLower(orig)] = id
		}
		ch.ID = id
		ch.Title = title
		ch.Description = desc
		out = append(out, ch)
	}

	valid := map[string]bool{}
	for _, ch := range out {
		valid[ch.ID] = true
	}
	for i := range out {
		var deps []string
		for _, d := range out[i].Contract.Dependencies {
			mapped := idMap[d]
			if mapped == "" {
				mapped = idMap[strings.ToLower(d)]
			}
			if mapped == "" {
				mapped = d
			}
			if mapped == out[i].ID {
				continue
			}
			if valid[mapped] {
				deps = append(deps, mapped)
			}
		}
		out[i].Contract.Dependencies = deps
	}
	return out, nil
}

func ensureDoD(dod *models.DoD, outputs []string, proj tools.ProjectType) {
	if dod.TimeoutSec <= 0 {
		dod.TimeoutSec = 60
	}
	if len(dod.Commands) == 0 {
		dod.Commands = defaultDoDCommands(outputs, proj)
	}
}

// ensureGoalDoD fills a compound node's goal-level acceptance DoD. When the
// model provided no commands, fall back to a toolchain-appropriate build; for
// Go, "go build ." fails when no entry package exists, catching a missing
// integration leaf, and a whole-tree build backs it up.
func ensureGoalDoD(dod *models.DoD, proj tools.ProjectType) {
	if dod.TimeoutSec <= 0 {
		dod.TimeoutSec = 120
	}
	if len(dod.Commands) == 0 {
		switch proj {
		case tools.ProjectGo:
			dod.Commands = []string{"go build .", "go build ./..."}
		case tools.ProjectNode:
			// The model's real goal acceptance should start the server and
			// curl it; as a fallback at least exercise the declared tests.
			dod.Commands = []string{"npm test --if-present"}
		case tools.ProjectRust:
			dod.Commands = []string{"cargo build"}
		case tools.ProjectPython:
			dod.Commands = []string{pythonCheckCmd}
		default:
			dod.Commands = []string{"ls"}
		}
	}
}

func mergeContract(a, b models.ContractSpec) models.ContractSpec {
	if len(a.Inputs)+len(a.Outputs)+len(a.Constraints)+len(a.Dependencies) == 0 {
		return b
	}
	return a
}

func mergeDoD(a, b models.DoD) models.DoD {
	if a.Description == "" && len(a.Commands) == 0 {
		return b
	}
	return a
}

func formatContract(c models.ContractSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  inputs: %s\n", joinOrDash(c.Inputs))
	fmt.Fprintf(&b, "  outputs: %s\n", joinOrDash(c.Outputs))
	fmt.Fprintf(&b, "  dependencies: %s\n", joinOrDash(c.Dependencies))
	fmt.Fprintf(&b, "  constraints: %s\n", joinOrDash(c.Constraints))
	return b.String()
}

func formatDoD(d models.DoD) string {
	var b strings.Builder
	if d.Description != "" {
		fmt.Fprintf(&b, "  description: %s\n", d.Description)
	}
	if len(d.Commands) == 0 {
		b.WriteString("  commands: (none)\n")
	} else {
		b.WriteString("  commands:\n")
		for _, c := range d.Commands {
			fmt.Fprintf(&b, "    - %s\n", c)
		}
	}
	if d.ExpectedOutput != "" {
		fmt.Fprintf(&b, "  expected_output: %s\n", d.ExpectedOutput)
	}
	return b.String()
}

func joinOrDash(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}
