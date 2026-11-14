package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/tools"
)

func TestCanonicalizeToolNames(t *testing.T) {
	acts := []taggedAction{
		{Name: "edit"},
		{Name: "read"},
		{Name: "read_file"},
		{Name: "mcp_server_read"},
	}
	canonicalizeToolNames(acts, func(name string) bool { return name == "mcp_server_read" })
	want := []string{tools.ToolReplaceLines, tools.ToolReadFile, tools.ToolReadFile, "mcp_server_read"}
	for i, w := range want {
		if acts[i].Name != w {
			t.Fatalf("action %d: got %q, want %q", i, acts[i].Name, w)
		}
	}
}

// Native tool_calls arrive with whatever name the model emitted: there is no
// ParseAction normalizing "done" to "finish" on that path. Resolving at
// dispatch alone is too late — finish interception keys off the name, so the
// turn would keep running until the step budget and report failure.
func TestDirect_AliasedNamesDriveTheLoop(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	cfg := directCfg(t, work, data, "Create a note file and stop.")
	cfg.NativeTools = true

	calls := 0
	client := &llm.ScriptedClient{Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function",
				Function: llm.ToolFunction{
					Name:      "write",
					Arguments: `{"path":"note.txt","content":"hello"}`,
				},
			}}}, nil
		}
		return &llm.Response{ToolCalls: []llm.ToolCall{{
			ID: "call_2", Type: "function",
			Function: llm.ToolFunction{
				Name:      "done",
				Arguments: `{"summary":"created note.txt with one line"}`,
			},
		}}}, nil
	}}

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	if err := NewDirect(o, nil).Run(context.Background()); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "note.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("the aliased write must reach the file: %q, %v", got, err)
	}
	// Two model calls means "done" was recognized as finish; anything else
	// would keep the loop alive until the step budget (20 here).
	if calls != 2 {
		t.Fatalf(`the aliased "done" must end the turn after 2 calls, made %d`, calls)
	}
	raw, err := os.ReadFile(filepath.Join(data, o.SessionID()+".direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"done": true`) {
		t.Fatalf("transcript should record a finished turn:\n%s", raw)
	}
	if !strings.Contains(string(raw), "created note.txt") {
		t.Fatalf("the summary must come from the model's finish args")
	}
}
