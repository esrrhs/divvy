package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/tools"
)

func writeRuleFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRules_EmptyWorkspace(t *testing.T) {
	sb, err := tools.NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := loadProjectRules(sb); got != "" {
		t.Fatalf("empty workspace should produce no rules block, got %q", got)
	}
	if strings.Contains(directSystemGuide(sb), "PROJECT RULES") {
		t.Fatal("direct prompt should not announce rules when the project ships none")
	}
}

func TestProjectRules_LoadsOrderedAndDeduplicated(t *testing.T) {
	root := t.TempDir()
	writeRuleFile(t, root, "AGENTS.md", "Use tabs, not spaces.")
	// Same content under another name must not double the prompt.
	writeRuleFile(t, root, "DIVVY.md", "Use tabs, not spaces.")
	writeRuleFile(t, root, ".divvy/rules/b.md", "Keep the public API stable.")
	writeRuleFile(t, root, ".divvy/rules/a.md", "Prefer table-driven tests.")
	writeRuleFile(t, root, ".divvy/rules/notes.txt", "This is not a rule file.")

	sb, err := tools.NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := loadProjectRules(sb)
	for _, want := range []string{
		"PROJECT RULES",
		"### AGENTS.md",
		"### .divvy/rules/a.md",
		"### .divvy/rules/b.md",
		"Use tabs, not spaces.",
		"Prefer table-driven tests.",
		"Keep the public API stable.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rules block missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "Use tabs, not spaces.") != 1 {
		t.Fatalf("duplicate rule content should appear once:\n%s", got)
	}
	if strings.Contains(got, "notes.txt") || strings.Contains(got, "not a rule file") {
		t.Fatalf("non-markdown files must be ignored:\n%s", got)
	}
	if strings.Index(got, "### AGENTS.md") > strings.Index(got, "### .divvy/rules/a.md") {
		t.Fatalf("root AGENTS.md should lead the block:\n%s", got)
	}
	if strings.Index(got, "### .divvy/rules/a.md") > strings.Index(got, "### .divvy/rules/b.md") {
		t.Fatalf("rule directory should be lexical:\n%s", got)
	}
}

func TestProjectRules_TruncatesOversizedFile(t *testing.T) {
	root := t.TempDir()
	writeRuleFile(t, root, "AGENTS.md", strings.Repeat("x", projectRulesMaxBytes+5000))
	sb, err := tools.NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := loadProjectRules(sb)
	if !strings.Contains(got, "truncated at 16384 bytes") {
		t.Fatalf("oversized rules should be truncated:\n%s", got[:min(len(got), 300)])
	}
	if len(got) > projectRulesMaxBytes+1000 {
		t.Fatalf("rules block should stay bounded, got %d bytes", len(got))
	}
}

func TestProjectRules_InjectedIntoEveryLoopPrompt(t *testing.T) {
	root := t.TempDir()
	writeRuleFile(t, root, "AGENTS.md", "Always run go test ./... before finishing.")
	sb, err := tools.NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	const rule = "Always run go test ./... before finishing."
	if !strings.Contains(directSystemGuide(sb), rule) {
		t.Fatal("direct prompt must carry project rules")
	}
	if !strings.Contains(workerSystemFor(false, "")+projectRulesBlock(sb), rule) {
		t.Fatal("worker prompt must carry project rules")
	}
	if !strings.Contains(soloSystemFor(false, "")+projectRulesBlock(sb), rule) {
		t.Fatal("solo prompt must carry project rules")
	}
}
