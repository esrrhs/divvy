package tools

import (
	"strings"
	"testing"
)

func TestCanonicalToolName(t *testing.T) {
	cases := map[string]string{
		"cat":           ToolReadFile,
		"batch_read":    ToolReadFile,
		"edit":          ToolReplaceLines,
		"bash":          ToolRunBash,
		"grep":          ToolSearchFiles,
		"done":          ToolFinish,
		" READ_FILE":    ToolReadFile,    // already canonical; only normalized
		"mcp__fs__read": "mcp__fs__read", // server-owned names pass through
		"whatever":      "whatever",
	}
	for in, want := range cases {
		if got := CanonicalToolName(in); got != want {
			t.Errorf("CanonicalToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// An invented name must reach the real tool, and the model must be told its
// name was wrong so it does not keep inventing it.
func TestCall_DispatchesAlias(t *testing.T) {
	sb := newAnchorSandbox(t)
	if err := sb.WriteFile("calc.go", goSource); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(t.Context(), "cat", map[string]any{"path": "calc.go"})
	if err != nil {
		t.Fatalf("alias should dispatch, got %v", err)
	}
	if !strings.Contains(out, "func Average") {
		t.Fatalf("alias must return the real tool's result, got %q", out)
	}
	// The result is the real tool's, unwrapped: the alias is a compatibility
	// shim, not something the model has to read about on every call.
	if strings.Contains(out, "[alias]") {
		t.Fatalf("alias notes must not pollute tool results, got %q", out)
	}
}

// The alias must inherit the target's policy: `bash` when run_bash is disabled
// is unavailable, not "unknown".
func TestCall_AliasInheritsPolicy(t *testing.T) {
	sb := newAnchorSandbox(t)
	sb.DisableTool(ToolRunBash, "run_bash is disabled in this environment")
	_, err := sb.Call(t.Context(), "bash", map[string]any{"command": "true"})
	if err == nil {
		t.Fatal("an alias of a disabled tool must be refused")
	}
	if strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("alias must resolve before the policy check, got %v", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want the disabled reason, got %v", err)
	}
}

func TestCall_UnknownToolSuggestsClosest(t *testing.T) {
	sb := newAnchorSandbox(t)
	_, err := sb.Call(t.Context(), "reed_file", map[string]any{"path": "a.go"})
	if err == nil {
		t.Fatal("a typo must not dispatch")
	}
	if !strings.Contains(err.Error(), "did you mean read_file?") {
		t.Fatalf("want a suggestion, got %v", err)
	}
	// Nonsense gets the list but no misleading guess.
	_, err = sb.Call(t.Context(), "zzzq", map[string]any{})
	if err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("no suggestion expected, got %v", err)
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("the message must still list the real tools, got %v", err)
	}
}

func TestSuggestTool(t *testing.T) {
	if got := suggestTool("write_fil"); got != ToolWriteFile {
		t.Fatalf("one-character drop should suggest write_file, got %q", got)
	}
	if got := suggestTool("run_bas"); got != ToolRunBash {
		t.Fatalf("want run_bash, got %q", got)
	}
	if got := suggestTool("xyz"); got != "" {
		t.Fatalf("short nonsense must not produce a suggestion, got %q", got)
	}
}
