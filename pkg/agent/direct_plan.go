package agent

import (
	"fmt"
	"strings"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

// directPlanMaxItems bounds the task list. A plan with more steps than this
// stops being orientation and becomes a second transcript the model has to
// keep in sync.
const directPlanMaxItems = 12

// directPlanItem is one step of the model-maintained task list. Statuses are
// normalized so a model writing "done" or "todo" still produces a usable plan.
type directPlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

const (
	planPending    = "pending"
	planInProgress = "in_progress"
	planCompleted  = "completed"
	planCancelled  = "cancelled"
)

// directPlanNativeTool exposes update_plan only to the direct loop. Workers
// and solo runs have bounded scopes where a persistent plan adds protocol
// weight without helping.
func directPlanNativeTool() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        tools.ToolUpdatePlan,
			Description: "Replace the current task list. Use it for multi-step work, keep exactly one step in_progress, and mark steps completed as you verify them. finish is rejected while a step is pending or in progress.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"items": map[string]any{
						"type":        "array",
						"description": "Complete replacement list, ordered",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"step":   map[string]any{"type": "string", "description": "Short description of the step"},
								"status": map[string]any{"type": "string", "description": "pending | in_progress | completed | cancelled"},
							},
							"required": []string{"step", "status"},
						},
					},
				},
				"required": []string{"items"},
			},
		},
	}
}

func directPlanGuide() string {
	return "- update_plan: {\"items\":[{\"step\":\"...\",\"status\":\"pending|in_progress|completed|cancelled\"}]}  (replace the whole plan; keep at most one in_progress; finish is rejected while any step is pending or in progress)\n"
}

// normalizePlanStatus accepts the vocabulary weak models actually emit.
func normalizePlanStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case planPending, "todo", "open", "not_started", "not started":
		return planPending, true
	case planInProgress, "doing", "active", "started", "in-progress":
		return planInProgress, true
	case planCompleted, "done", "complete", "finished":
		return planCompleted, true
	case planCancelled, "canceled", "skipped", "dropped":
		return planCancelled, true
	}
	return "", false
}

// parsePlanItems validates a whole-plan replacement from tool arguments. The
// errors are written for the model: they name the offending index and the
// accepted statuses so a repair is possible without guessing.
func parsePlanItems(raw any) ([]directPlanItem, error) {
	list, ok := raw.([]any)
	if !ok {
		if typed, ok := raw.([]directPlanItem); ok {
			list = make([]any, 0, len(typed))
			for _, it := range typed {
				list = append(list, map[string]any{"step": it.Step, "status": it.Status})
			}
		} else {
			return nil, fmt.Errorf("items must be an array of {step,status} objects")
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("items must not be empty; omit update_plan entirely when there is nothing to track")
	}
	if len(list) > directPlanMaxItems {
		return nil, fmt.Errorf("items has %d entries; keep at most %d", len(list), directPlanMaxItems)
	}
	out := make([]directPlanItem, 0, len(list))
	inProgress := 0
	for i, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("items[%d] must be an object with step and status", i)
		}
		step, _ := stringFromArgs(m, "step")
		step = strings.TrimSpace(step)
		if step == "" {
			return nil, fmt.Errorf("items[%d].step must not be empty", i)
		}
		if len(step) > 240 {
			step = step[:240] + "…"
		}
		statusRaw, _ := stringFromArgs(m, "status")
		status, ok := normalizePlanStatus(statusRaw)
		if !ok {
			return nil, fmt.Errorf("items[%d].status %q is invalid; use pending, in_progress, completed, or cancelled", i, statusRaw)
		}
		if status == planInProgress {
			inProgress++
		}
		out = append(out, directPlanItem{Step: step, Status: status})
	}
	if inProgress > 1 {
		return nil, fmt.Errorf("%d steps are in_progress; mark at most one as in_progress so the current focus is unambiguous", inProgress)
	}
	return out, nil
}

// updatePlan applies an update_plan call and returns the confirmation shown
// to the model on the next step.
func (d *DirectAgent) updatePlan(args map[string]any) (string, error) {
	raw, ok := args["items"]
	if !ok {
		return "", fmt.Errorf("items is required")
	}
	items, err := parsePlanItems(raw)
	if err != nil {
		return "", err
	}
	d.st.Plan = items
	d.o.events.Record("direct_plan", d.o.tree.RootID, map[string]any{
		"items":     len(items),
		"completed": countPlanStatus(items, planCompleted),
		"pending":   countPlanStatus(items, planPending) + countPlanStatus(items, planInProgress),
	})
	return "Plan updated:\n" + renderPlan(items), nil
}

func countPlanStatus(items []directPlanItem, status string) int {
	n := 0
	for _, it := range items {
		if it.Status == status {
			n++
		}
	}
	return n
}

// renderPlan lists the plan as the model sees it in tool results and in the
// system prompt. Markers stay textual so they survive JSON and native calls.
func renderPlan(items []directPlanItem) string {
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, it.Status, it.Step)
		if i < len(items)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// directPlanBlock is appended to the system prompt on every step. It keeps the
// plan visible after transcript trimming and makes the finish rule explicit.
func directPlanBlock(items []directPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	return "\n\nCURRENT PLAN (replace it with update_plan as work advances; finish is rejected while any step is pending or in progress):\n" + renderPlan(items)
}

// unfinishedPlanBlock is the deterministic finish gate. Without a plan the
// model may finish freely; once it created one, it must either complete the
// remaining steps or explicitly cancel them (cancelled is a decision, pending
// is an omission).
func (d *DirectAgent) unfinishedPlanBlock() (string, bool) {
	var open []string
	for i, it := range d.st.Plan {
		if it.Status == planPending || it.Status == planInProgress {
			open = append(open, fmt.Sprintf("%d. [%s] %s", i+1, it.Status, it.Step))
		}
	}
	if len(open) == 0 {
		return "", true
	}
	return "finish rejected: the plan still has unfinished steps.\n" +
		strings.Join(open, "\n") +
		"\n\nFinish the work and mark the steps completed, or call update_plan again marking steps you are deliberately not doing as cancelled. Do not just call finish again.", false
}
