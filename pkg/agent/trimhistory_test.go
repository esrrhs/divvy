package agent

import (
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
)

// toolCallMsg builds an assistant turn that requested one native tool call.
func toolCallMsg(id string) llm.Message {
	return llm.Message{
		Role:    llm.RoleAssistant,
		Content: "",
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.ToolFunction{Name: "read_file", Arguments: `{"path":"a.go"}`},
		}},
	}
}

func toolResultMsg(id string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: "read_file", Content: "ok"}
}

// orphanToolResults walks a message slice the way an OpenAI-compatible server
// validates it: every role:"tool" message must be preceded by an assistant
// turn carrying a matching tool_call id. It returns the offending ids.
func orphanToolResults(messages []llm.Message) []string {
	answered := map[string]bool{}
	var orphans []string
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			answered[tc.ID] = false
		}
		if m.Role == llm.RoleTool {
			if seen, ok := answered[m.ToolCallID]; !ok || seen {
				orphans = append(orphans, m.ToolCallID)
			} else {
				answered[m.ToolCallID] = true
			}
		}
	}
	return orphans
}

// TestTrimHistoryKeepsToolCallsPaired is the regression test for the defect
// where trimming cut between an assistant tool_calls turn and its tool
// results, making the server reject the whole request with 400.
func TestTrimHistoryKeepsToolCallsPaired(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "task"},
		toolCallMsg("c1"),
		toolResultMsg("c1"),
		{Role: llm.RoleAssistant, Content: "thinking"},
		{Role: llm.RoleUser, Content: "Tool read_file result:\nfine"},
	}

	// Every max must yield a server-acceptable sequence.
	for max := 2; max <= 8; max++ {
		out := trimHistory(msgs, max)
		if len(out) > max && max > 2 {
			// Trimming is allowed to exceed max only to preserve a tool group,
			// which is exactly the intended trade-off.
			t.Logf("max=%d kept %d messages (tool group preserved)", max, len(out))
		}
		if orphans := orphanToolResults(out); len(orphans) > 0 {
			t.Errorf("max=%d orphaned tool results: %v", max, orphans)
		}
		if out[0].Role != llm.RoleSystem || out[1].Role != llm.RoleUser {
			t.Errorf("max=%d head not preserved: %q/%q", max, out[0].Role, out[1].Role)
		}
	}
}

// TestTrimHistoryMultipleToolCallsInOneTurn covers a single assistant turn that
// requested several tools: the cut must not land between the results.
func TestTrimHistoryMultipleToolCallsInOneTurn(t *testing.T) {
	multi := llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{
			{ID: "a", Type: "function", Function: llm.ToolFunction{Name: "read_file", Arguments: "{}"}},
			{ID: "b", Type: "function", Function: llm.ToolFunction{Name: "list_dir", Arguments: "{}"}},
			{ID: "c", Type: "function", Function: llm.ToolFunction{Name: "search_files", Arguments: "{}"}},
		},
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "task"},
		multi,
		toolResultMsg("a"),
		toolResultMsg("b"),
		toolResultMsg("c"),
		{Role: llm.RoleAssistant, Content: "done"},
	}

	for max := 3; max <= 8; max++ {
		out := trimHistory(msgs, max)
		if orphans := orphanToolResults(out); len(orphans) > 0 {
			t.Errorf("max=%d orphaned tool results: %v", max, orphans)
		}
	}
}

// TestTrimHistoryNoopWhenShort verifies the common case is untouched.
func TestTrimHistoryNoopWhenShort(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "task"},
	}
	out := trimHistory(msgs, 18)
	if len(out) != 2 {
		t.Fatalf("short history should pass through unchanged, got %d messages", len(out))
	}
}

// TestTrimHistoryPlainJSONModeUnaffected guards the default (non-native) path:
// with no tool calls present, trimming must still keep exactly max messages.
func TestTrimHistoryPlainJSONModeUnaffected(t *testing.T) {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: "sys"})
	for i := 0; i < 30; i++ {
		if i%2 == 0 {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "x"})
		} else {
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "y"})
		}
	}
	out := trimHistory(msgs, 18)
	if len(out) != 18 {
		t.Fatalf("plain JSON mode should keep exactly 18 messages, got %d", len(out))
	}
}
