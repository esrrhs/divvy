package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigMergesAndOverrides(t *testing.T) {
	data, work := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(data, "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "user-cmd", "args": ["a"]},
			"user-only": {"command": "u"},
			"off": {"command": "x", "disabled": true}
		}
	}`)
	writeJSON(t, filepath.Join(work, ".divvy", "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "project-cmd"},
			"project-only": {"command": "p", "env": {"TOKEN": "t"}}
		}
	}`)
	cfg, err := LoadConfig(data, work)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["shared"].Command != "project-cmd" {
		t.Fatalf("project config should override user config, got %+v", cfg["shared"])
	}
	if _, ok := cfg["user-only"]; !ok {
		t.Fatal("user-only server missing")
	}
	if _, ok := cfg["project-only"]; !ok {
		t.Fatal("project-only server missing")
	}
	if _, ok := cfg["off"]; ok {
		t.Fatal("disabled server should be dropped")
	}
	if got := ServerNames(cfg); strings.Join(got, ",") != "project-only,shared,user-only" {
		t.Fatalf("server names should be sorted, got %v", got)
	}
}

func TestLoadConfigMissingAndMalformed(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	if err != nil || len(cfg) != 0 {
		t.Fatalf("missing config should be an empty success: cfg=%v err=%v", cfg, err)
	}
	data := t.TempDir()
	writeJSON(t, filepath.Join(data, "mcp.json"), `{"mcpServers":`)
	if _, err := LoadConfig(data, ""); err == nil {
		t.Fatal("malformed config should report an error")
	}
	writeJSON(t, filepath.Join(data, "mcp.json"), `{"mcpServers":{"bad":{"args":[]}}}`)
	if _, err := LoadConfig(data, ""); err == nil {
		t.Fatal("server without a command should report an error")
	}
}

func buildHelper(t *testing.T) string {
	t.Helper()
	name := "mcphelper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./testdata/mcphelper")
	cmd.Dir = "."
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mcp helper: %v\n%s", err, data)
	}
	return out
}

func startHelper(t *testing.T) *Runtime {
	t.Helper()
	rt, err := Start(context.Background(), map[string]ServerConfig{
		"helper": {Command: buildHelper(t)},
	}, t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(rt.Close)
	return rt
}

// TestRuntimeListsCallsAndPaginates covers the wrapper's core protocol work:
// initialize, paginated tools/list, prefixed names, flattened text results,
// and isError results surfacing as Go errors.
func TestRuntimeListsCallsAndPaginates(t *testing.T) {
	rt := startHelper(t)
	if !rt.HasTool("mcp__helper__echo") || !rt.HasTool("mcp__helper__fail") {
		t.Fatalf("both pages of tools should be indexed: %+v", rt.tools)
	}
	if len(rt.ToolDefs()) != 2 {
		t.Fatalf("expected 2 tool definitions, got %d", len(rt.ToolDefs()))
	}
	guide := rt.Guide()
	for _, want := range []string{"mcp__helper__echo", "Echo back", "text:string", "server: helper", "tool: echo"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide missing %q:\n%s", want, guide)
		}
	}

	ctx := context.Background()
	out, err := rt.Call(ctx, "mcp__helper__echo", map[string]any{"text": "hello"})
	if err != nil || out != "echo: hello" {
		t.Fatalf("echo call = %q, %v", out, err)
	}
	if out, err := rt.Call(ctx, "mcp__helper__fail", map[string]any{"message": "nope"}); err == nil || !strings.Contains(err.Error(), "boom: nope") {
		t.Fatalf("isError result should become an error with its text: out=%q err=%v", out, err)
	}
	if _, err := rt.Call(ctx, "mcp__nope__tool", nil); err == nil {
		t.Fatal("unknown tool should error")
	}
}

// TestRuntimeSkipsBrokenServer keeps one bad server from taking down the
// healthy ones; the failure is reported, not hidden.
func TestRuntimeSkipsBrokenServer(t *testing.T) {
	rt, err := Start(context.Background(), map[string]ServerConfig{
		"broken": {Command: filepath.Join(t.TempDir(), "does-not-exist")},
		"helper": {Command: buildHelper(t)},
	}, t.TempDir(), t.Logf)
	if err == nil {
		t.Fatal("a failed server should be reported")
	}
	if rt == nil || !rt.HasTool("mcp__helper__echo") {
		t.Fatalf("healthy server should still be usable: rt=%v err=%v", rt, err)
	}
	rt.Close()
}

// TestRuntimeCloseUnblocksCalls makes sure shutting a turn down does not leave
// a call waiting for a response that will never arrive.
func TestRuntimeCloseUnblocksCalls(t *testing.T) {
	rt := startHelper(t)
	if _, err := rt.Call(context.Background(), "mcp__helper__echo", map[string]any{"text": "before"}); err != nil {
		t.Fatal(err)
	}
	rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.Call(ctx, "mcp__helper__echo", map[string]any{"text": "after"}); err == nil {
		t.Fatal("call after Close should fail")
	}
}
