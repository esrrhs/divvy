package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

func (o *Orchestrator) runWorker(ctx context.Context, sb *tools.Sandbox, node *models.TaskNode, prevError string) (string, error) {
	// Release the leaf-level browser lease no matter how the worker ends,
	// so a queued parallel leaf is never stuck waiting after a failure.
	defer sb.ReleaseBrowser()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: workerSystemFor(o.cfg.WebEnabled, sb.DynamicToolDescriptions())},
		{Role: llm.RoleUser, Content: o.projectContext(sb, node, prevError)},
	}

	var native []llm.Tool
	if o.cfg.NativeTools {
		native = nativeToolDefs(sb)
	}

	maxSteps := o.cfg.MaxSteps
	if maxSteps < 1 {
		maxSteps = 12
	}

	summary := ""
	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		o.log.Actionf("worker %s step %d/%d", node.ID, step, maxSteps)

		resp, err := o.chat(ctx, "worker", node.ID, llm.Request{
			Model:       o.cfg.Model,
			Messages:    o.stepBudgetMessages(messages, step, maxSteps),
			Tools:       native,
			Temperature: o.cfg.Temperature,
			MaxTokens:   o.cfg.MaxTokens,
			Stream:      o.cfg.Stream,
		})
		if err != nil {
			return summary, err
		}
		o.log.Debugf("worker raw: %s", truncate(resp.Content, 400))

		actions := collectActions(resp)
		if len(actions) == 0 {
			messages = append(messages,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Content},
				llm.Message{Role: llm.RoleUser, Content: `Could not parse a tool call. Reply with ONLY JSON: {"thought":"...","action":"tool_name","args":{}}`},
			)
			continue
		}

		// Record assistant turn.
		asst := llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls}
		messages = append(messages, asst)

		for _, act := range actions {
			o.log.Actionf("%s %s %s", node.ID, act.Name, previewArgs(act))
			if act.Name == tools.ToolFinish {
				// Deterministic pre-finish gate (git workspaces): conflict
				// markers and hard-coded secrets reject finish so the leaf
				// must fix them in another turn; debug prints / large diffs
				// are only logged as warnings.
				gate, gateErr := sb.PreFinishGate(ctx)
				if gateErr != nil {
					o.log.Warnf("pre-finish review unavailable: %v", gateErr)
				} else if gate != nil {
					if gate.Blocking {
						o.log.Warnf("%s finish rejected by review gate", node.ID)
						o.events.Record("finish_gate", node.ID, map[string]any{
							"blocking": true,
							"report":   clip(gate.Report, evReasonChars),
						})
						blocked := "finish REJECTED by review_diff. Fix these blocking issue(s) before finishing (conflict markers and hard-coded secrets are never accepted):\n" +
							gate.Report +
							"\nFix the code, then call finish again. Debug-print and large-diff items are warnings only, but conflict/secret items must be removed."
						if o.cfg.NativeTools && act.ID != "" {
							messages = append(messages, llm.Message{
								Role: llm.RoleTool, ToolCallID: act.ID, Name: act.Name, Content: blocked,
							})
						} else {
							messages = append(messages, llm.Message{
								Role: llm.RoleUser, Content: "Tool finish result:\n" + blocked,
							})
						}
						continue
					}
					o.log.Warnf("%s pre-finish warnings:\n%s", node.ID, gate.Report)
					o.events.Record("finish_gate", node.ID, map[string]any{
						"blocking": false,
						"report":   clip(gate.Report, evReasonChars),
					})
				}

				summary, _ = stringFromArgs(act.Args, "summary")
				if summary == "" {
					summary = act.Thought
				}
				if summary == "" {
					summary = "finished"
				}
				return summary, nil
			}

			// "ask" questions the user mid-task (only available in a guided
			// interactive session). In batch mode there is no user; tell the
			// model to make a reasonable assumption and continue.
			if act.Name == "ask" {
				question, _ := stringFromArgs(act.Args, "question")
				var answer string
				if o.askHook == nil {
					answer = "No interactive user is available in this run. Make a reasonable assumption and continue; do not call ask again."
				} else {
					o.log.Actionf("%s asks: %s", node.ID, question)
					askStart := time.Now()
					answer = o.askHook(question)
					o.events.Record("ask", node.ID, map[string]any{
						"question":    clip(question, evReasonChars),
						"answer":      clip(answer, evReasonChars),
						"duration_ms": time.Since(askStart).Milliseconds(),
					})
				}
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: fmt.Sprintf("User answer:\n%s", answer),
				})
				continue
			}

			callStart := time.Now()
			out, callErr := sb.Call(ctx, act.Name, act.Args)
			o.events.ToolCall(node.ID, act.Name, act.Args, out, callErr, callStart)
			if callErr != nil {
				out = "ERROR: " + callErr.Error()
				o.log.Warnf("%s", out)
			} else {
				o.log.Debugf("tool out: %s", truncate(out, 240))
			}

			if o.cfg.NativeTools && act.ID != "" {
				messages = append(messages, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: act.ID,
					Name:       act.Name,
					Content:    out,
				})
			} else {
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: fmt.Sprintf("Tool %s result:\n%s", act.Name, out),
				})
			}
		}

		messages = trimHistory(messages, 18)
	}
	if summary == "" {
		summary = "max steps reached"
	}
	// Running out of steps is a failure, not a finish: the model never
	// claimed the work was done, so its files may be half-written and the
	// pre-finish review gate never ran. Returning nil here used to send the
	// attempt straight to verification, where a placeholder DoD (`ls`)
	// could mark an unfinished leaf COMPLETED.
	return summary, fmt.Errorf("%w: used all %d tool calls without calling finish — on the next attempt, call finish as soon as the task is done instead of starting another edit", ErrMaxSteps, maxSteps)
}

// ErrMaxSteps marks a leaf attempt that exhausted its tool-call budget
// without the model calling finish. Callers treat it like any other attempt
// failure (retry, then re-split or give up), never as success.
var ErrMaxSteps = errors.New("max tool calls reached")

// stepBudgetWindow is how many calls before the end of the budget the worker
// starts telling the model what it has left. Small models lose track of the
// turn count long before the last call, so a late reminder is worth little.
const stepBudgetWindow = 3

// stepBudgetMessages returns the messages to send for one worker step. In the
// last few steps it appends a reminder of how many tool calls remain, which
// is the difference between a weak model calling finish in time and losing
// the whole attempt. The reminder is added to a copy: it must not become part
// of the persisted history, or every later step would inherit a stale count.
func (o *Orchestrator) stepBudgetMessages(messages []llm.Message, step, maxSteps int) []llm.Message {
	remaining := maxSteps - step + 1
	if remaining > stepBudgetWindow || remaining <= 0 {
		return messages
	}
	hint := llm.Message{
		Role: llm.RoleUser,
		Content: fmt.Sprintf("[budget] %d tool call(s) left before this attempt is abandoned. "+
			"Call finish now with a summary of what is done; do not start another edit.", remaining),
	}
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, messages...)
	out = append(out, hint)
	return out
}

type taggedAction struct {
	ID      string
	Name    string
	Thought string
	Args    map[string]any
}

func collectActions(resp *llm.Response) []taggedAction {
	var out []taggedAction
	for _, tc := range resp.ToolCalls {
		args := map[string]any{}
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			_ = llm.UnmarshalFlexible(tc.Function.Arguments, &args)
		}
		out = append(out, taggedAction{
			ID:   tc.ID,
			Name: strings.ToLower(tc.Function.Name),
			Args: args,
		})
	}
	if len(out) > 0 {
		return out
	}
	if strings.TrimSpace(resp.Content) == "" {
		return nil
	}
	act, err := llm.ParseAction(resp.Content)
	if err != nil {
		return nil
	}
	return []taggedAction{{Name: act.Name, Thought: act.Thought, Args: act.Args}}
}

func nativeToolDefs(sb *tools.Sandbox) []llm.Tool {
	raw := tools.NativeTools()
	raw = append(raw, tools.NativeCodeTools()...)
	if tools.IsRepo(sb.Root) {
		raw = append(raw, tools.NativeGitTools()...)
	}
	if sb.Web != nil {
		raw = append(raw, tools.NativeWebTools()...)
		raw = append(raw, tools.NativeHTTPTool())
		raw = append(raw, tools.NativeDownloadTool())
	}
	if sb.Browser != nil {
		raw = append(raw, tools.NativeBrowserTools()...)
	}
	out := make([]llm.Tool, 0, len(raw))
	for _, m := range raw {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var t llm.Tool
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

func previewArgs(act taggedAction) string {
	switch act.Name {
	case tools.ToolReadFile, tools.ToolWriteFile, tools.ToolReplaceLines, tools.ToolListDir, tools.ToolDeletePath, tools.ToolOutline:
		p, _ := stringFromArgs(act.Args, "path")
		return p
	case tools.ToolMovePath:
		from, _ := stringFromArgs(act.Args, "from")
		to, _ := stringFromArgs(act.Args, "to")
		return truncate("move "+from+" -> "+to, 80)
	case tools.ToolDownloadFile:
		u, _ := stringFromArgs(act.Args, "url")
		p, _ := stringFromArgs(act.Args, "path")
		return truncate("download "+u+" -> "+p, 80)
	case tools.ToolSQLiteQuery, tools.ToolJSONQuery:
		p, _ := stringFromArgs(act.Args, "path")
		return p
	case tools.ToolRunBash:
		c, _ := stringFromArgs(act.Args, "command")
		return truncate(c, 80)
	case tools.ToolSearchFiles:
		p, _ := stringFromArgs(act.Args, "pattern")
		gl, _ := stringFromArgs(act.Args, "glob")
		return truncate(p+" ["+gl+"]", 80)
	case tools.ToolFindFiles:
		p, _ := stringFromArgs(act.Args, "pattern")
		dir, _ := stringFromArgs(act.Args, "path")
		return truncate(p+" ["+dir+"]", 80)
	case tools.ToolFinish:
		s, _ := stringFromArgs(act.Args, "summary")
		return truncate(s, 80)
	default:
		return ""
	}
}

func stringFromArgs(args map[string]any, key string) (string, bool) {
	if args == nil {
		return "", false
	}
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// trimHistory caps the conversation to roughly max messages. It preserves two
// invariants every OpenAI-compatible server enforces:
//
//  1. the system prompt and the original task turn stay at the head;
//  2. the retained tail never splits a tool-call group — a role:"tool"
//     message must follow the assistant turn whose tool_calls it answers.
//
// Violating (2) makes the server reject the whole request with 400, which
// costs a leaf its entire context and burns a retry. The cut is therefore
// walked backwards to the nearest boundary that keeps every tool result
// attached to its assistant turn.
func trimHistory(messages []llm.Message, max int) []llm.Message {
	if len(messages) <= max {
		return messages
	}
	// Keep system + original user + tail.
	head := 2
	if len(messages) < head {
		return messages
	}
	if max <= head {
		// No room for a tail; the head alone is still a valid request.
		return messages[:head]
	}
	cut := len(messages) - (max - head)
	if cut < head {
		cut = head
	}
	for cut > head && splitsToolGroup(messages, cut) {
		cut--
	}
	out := make([]llm.Message, 0, head+len(messages)-cut)
	out = append(out, messages[:head]...)
	out = append(out, messages[cut:]...)
	return out
}

// splitsToolGroup reports whether starting the retained tail at index cut
// would orphan a tool result or leave an assistant's tool_calls unanswered.
// Tool replies for one assistant turn are always contiguous in the worker
// loop, so the seam is safe when it is not a role:"tool" message and the
// preceding message is not an assistant awaiting replies.
func splitsToolGroup(messages []llm.Message, cut int) bool {
	if cut >= len(messages) {
		return false
	}
	if messages[cut].Role == llm.RoleTool {
		return true
	}
	prev := messages[cut-1]
	return prev.Role == llm.RoleAssistant && len(prev.ToolCalls) > 0
}

func (o *Orchestrator) projectContext(sb *tools.Sandbox, node *models.TaskNode, prevError string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n\n", sb.Root)
	fmt.Fprintf(&b, "Task ID: %s\nTitle: %s\nDescription:\n%s\n\n", node.ID, node.Title, node.Description)
	b.WriteString("Contract:\n")
	b.WriteString(formatContract(node.Contract))
	b.WriteString("\nVerification that will run after you finish:\n")
	b.WriteString(formatDoD(node.DoD))

	if node.ParentID != "" {
		if p, ok := o.tree.CloneNode(node.ParentID); ok {
			fmt.Fprintf(&b, "\nParent %s: %s\nParent contract:\n%s", p.ID, p.Title, formatContract(p.Contract))
		}
	}

	for _, depID := range node.Contract.Dependencies {
		if dep, ok := o.tree.CloneNode(depID); ok {
			fmt.Fprintf(&b, "\nDependency %s [%s]: %s\n", dep.ID, dep.State, dep.Title)
			if dep.ResultSummary != "" {
				fmt.Fprintf(&b, "  result: %s\n", truncate(dep.ResultSummary, 400))
			}
			if len(dep.Contract.Outputs) > 0 {
				fmt.Fprintf(&b, "  outputs: %s\n", strings.Join(dep.Contract.Outputs, ", "))
			}
		}
	}

	b.WriteString("\nWorkspace files:\n")
	b.WriteString(sb.Snapshot())

	injected := 0
	for _, in := range node.Contract.Inputs {
		if !looksLikePath(in) {
			continue
		}
		content, err := sb.ReadFile(in)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n%s\n", in, truncate(content, 4000))
		injected++
		if injected >= 4 {
			break
		}
	}

	if prevError != "" {
		fmt.Fprintf(&b, "\nPrevious attempt failed. Fix this error:\n%s\n", truncate(prevError, 4000))
	}
	b.WriteString("\nStart by listing or reading only what you need, then implement the task.")
	return b.String()
}
