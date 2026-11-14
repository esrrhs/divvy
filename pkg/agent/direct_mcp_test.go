package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
)

func buildMCPHelper(t *testing.T) string {
	t.Helper()
	name := "mcphelper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "../mcp/testdata/mcphelper")
	cmd.Dir = "."
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mcp helper: %v\n%s", err, data)
	}
	return out
}

func writeMCPConfig(t *testing.T, dataDir, command string) {
	t.Helper()
	path := filepath.Join(dataDir, "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"mcpServers":{"helper":{"command":` + strconvQuote(command) + `}}}`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

// TestDirectMCPToolsReachPromptAndLoop covers the integration seam: a server
// declared in mcp.json contributes tool definitions and a JSON-action guide to
// the direct prompt, and a call is routed to the server instead of the
// filesystem sandbox.
func TestDirectMCPToolsReachPromptAndLoop(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	writeMCPConfig(t, data, buildMCPHelper(t))
	cfg := directCfg(t, work, data, "use the helper")
	tree := engine.NewTaskTree("t-mcp", "root", "use the helper")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	d.setupMCP(context.Background())
	if d.mcp == nil {
		t.Fatal("MCP runtime was not created")
	}
	t.Cleanup(d.closeMCP)

	if !d.mcp.HasTool("mcp__helper__echo") {
		t.Fatalf("echo tool missing: %+v", d.mcp.ToolDefs())
	}
	defs := d.mcpToolDefs()
	found := false
	for _, def := range defs {
		if def.Function.Name == "mcp__helper__echo" {
			found = true
			if def.Function.Parameters == nil {
				t.Fatal("MCP tool lost its input schema")
			}
		}
	}
	if !found {
		t.Fatalf("native definitions missing echo: %+v", defs)
	}
	guide := d.mcpGuide()
	if !strings.Contains(guide, "mcp__helper__echo") || !strings.Contains(guide, "Echo back") {
		t.Fatalf("JSON-action guide missing MCP tool:\n%s", guide)
	}

	d.st = &directState{Version: 1, Messages: []llm.Message{{Role: llm.RoleSystem, Content: "system"}}}
	act := taggedAction{ID: "call-echo", Name: "mcp__helper__echo", Args: map[string]any{"text": "hello"}}
	finished, _, err := d.applyActions(context.Background(), o.tree.RootID, []taggedAction{act})
	if err != nil || finished {
		t.Fatalf("MCP call should not finish: finished=%v err=%v", finished, err)
	}
	last := d.st.Messages[len(d.st.Messages)-1].Content
	if !strings.Contains(last, "echo: hello") {
		t.Fatalf("MCP result not appended to transcript: %q", last)
	}
	if d.st.InFlight != nil {
		t.Fatalf("in-flight marker should be cleared: %+v", d.st.InFlight)
	}
}

// TestDirectMCPConfigFailureFailsOpen keeps a bad config from blocking the
// turn: the agent logs a warning and continues with built-in tools.
func TestDirectMCPConfigFailureFailsOpen(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "mcp.json"), []byte(`{"mcpServers":`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := directCfg(t, work, data, "goal")
	tree := engine.NewTaskTree("t-mcp-open", "root", "goal")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	d := NewDirect(o, nil)
	d.setupMCP(context.Background())
	if d.mcp != nil {
		d.closeMCP()
	}
	// The point is that setupMCP returned; a malformed config must not panic
	// or prevent the loop from starting with built-in tools.
	if !strings.Contains(directSystemGuide(d.o.sandbox, data), "run_bash") {
		t.Fatal("built-in tools should still be described")
	}
}
