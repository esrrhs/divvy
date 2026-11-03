package agent

import (
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func TestCollectActions_MissingWriteContentReasks(t *testing.T) {
	resp := &llm.Response{Content: `{"thought":"w","action":"write_file","args":{"path":"a.go"}}`}
	acts, hint := collectActions(resp)
	if len(acts) != 0 {
		t.Fatalf("incomplete write_file must not execute, got %#v", acts)
	}
	if !strings.Contains(hint, "content") {
		t.Fatalf("hint should demand content: %q", hint)
	}
}

func TestCollectActions_TruncatedJSONReasks(t *testing.T) {
	resp := &llm.Response{Content: `{"thought":"w","action":"write_file","args":{"path":"a.go","content":"pack`}
	acts, hint := collectActions(resp)
	// Either repaired into a complete action or same-turn re-ask hint.
	if len(acts) == 0 && hint == "" {
		t.Fatal("truncated write_file must repair or re-ask")
	}
	if hint != "" && !strings.Contains(strings.ToLower(hint), "json") && !strings.Contains(hint, "content") {
		t.Fatalf("unexpected hint: %q", hint)
	}
}

func TestMissingActionFieldsHint_RunBash(t *testing.T) {
	hint := missingActionFieldsHint(taggedAction{Name: tools.ToolRunBash, Args: map[string]any{}})
	if !strings.Contains(hint, "command") {
		t.Fatalf("hint=%q", hint)
	}
}
