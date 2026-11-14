package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/engine"
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
	if got := loadProjectRules(sb, ""); got != "" {
		t.Fatalf("empty workspace should produce no rules block, got %q", got)
	}
	if strings.Contains(directSystemGuide(sb, ""), "PROJECT RULES") {
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
	got := loadProjectRules(sb, "")
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
	got := loadProjectRules(sb, "")
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
	if !strings.Contains(directSystemGuide(sb, ""), rule) {
		t.Fatal("direct prompt must carry project rules")
	}
	if !strings.Contains(workerSystemFor(false, "")+projectRulesBlock(sb, ""), rule) {
		t.Fatal("worker prompt must carry project rules")
	}
	if !strings.Contains(soloSystemFor(false, "")+projectRulesBlock(sb, ""), rule) {
		t.Fatal("solo prompt must carry project rules")
	}
}

// TestProjectRules_UserRulesApplyEverywhere covers user-level rules: they live
// in the data directory (not in any repository) and must apply to every
// project, ahead of project-specific rules.
func TestProjectRules_UserRulesApplyEverywhere(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	writeRuleFile(t, data, "rules.md", "Explain trade-offs in Chinese.")
	writeRuleFile(t, root, "AGENTS.md", "Use tabs, not spaces.")
	sb, err := tools.NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := loadProjectRules(sb, data)
	if !strings.Contains(got, "Explain trade-offs in Chinese.") || !strings.Contains(got, "Use tabs, not spaces.") {
		t.Fatalf("both user and project rules should be present:\n%s", got)
	}
	if strings.Index(got, "Explain trade-offs") > strings.Index(got, "Use tabs") {
		t.Fatalf("user rules should lead the block:\n%s", got)
	}
}

// TestProjectRules_RepoRootAppliesToSubdirectory keeps a workspace opened at a
// package directory inside its repository: the repo-root AGENTS.md still
// applies, and it is not duplicated when the workspace is the repo root.
func TestProjectRules_RepoRootAppliesToSubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	initGitRepo(t, repo)
	writeRuleFile(t, repo, "AGENTS.md", "Monorepo-wide: use pnpm.")
	sub := filepath.Join(repo, "packages", "app")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeRuleFile(t, sub, "AGENTS.md", "App-specific: use React.")

	sb, err := tools.NewSandbox(sub)
	if err != nil {
		t.Fatal(err)
	}
	got := loadProjectRules(sb, "")
	if !strings.Contains(got, "Monorepo-wide: use pnpm.") || !strings.Contains(got, "App-specific: use React.") {
		t.Fatalf("repo-root and workspace rules should both apply:\n%s", got)
	}
	if strings.Index(got, "Monorepo-wide") > strings.Index(got, "App-specific") {
		t.Fatalf("repo-root rules should lead workspace rules:\n%s", got)
	}
	if strings.Count(got, "Monorepo-wide: use pnpm.") != 1 {
		t.Fatalf("repo-root rules duplicated:\n%s", got)
	}

	rootSB, err := tools.NewSandbox(repo)
	if err != nil {
		t.Fatal(err)
	}
	rootRules := loadProjectRules(rootSB, "")
	if strings.Count(rootRules, "Monorepo-wide: use pnpm.") != 1 {
		t.Fatalf("rules duplicated when workspace is repo root:\n%s", rootRules)
	}
}

// TestProjectRules_PlannerSeesRules keeps the plan itself inside the project
// constraints: a rule such as "no third-party dependencies" must shape the
// decomposition and its DoD commands, not only the leaf that implements them.
func TestProjectRules_PlannerSeesRules(t *testing.T) {
	root := t.TempDir()
	writeRuleFile(t, root, "AGENTS.md", "No third-party dependencies.")
	cfg := mgrCfg(root, t.TempDir(), "sess_rules_planner_"+t.Name())
	tree := engine.NewTaskTree("t-rules-plan", "root", "build it")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)
	node, ok := tree.CloneNode(tree.RootID)
	if !ok {
		t.Fatal("root missing")
	}
	if prompt := o.decomposeUserPrompt(node); !strings.Contains(prompt, "No third-party dependencies.") {
		t.Fatalf("planner prompt must carry project rules:\n%s", prompt)
	}
}
