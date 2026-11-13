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

// TestDirectProjectMapRespectsGitignore keeps the onboarding map from
// advertising ignored build output and local-only directories. The model
// should see the project, not `target/`, `node_modules/` or a private notes
// directory that git already excludes.
func TestDirectProjectMapRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	writeFile := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(".gitignore", "private/\n*.tmp\n")
	writeFile("app.go", "package main\n")
	writeFile("src/main.go", "package main\n")
	writeFile("private/notes.md", "local only\n")
	writeFile("scratch.tmp", "generated\n")

	sb, err := tools.NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := directProjectMap(sb, 80, 3)
	for _, want := range []string{"app.go", "src/", "src/main.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("project map missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"private", "scratch.tmp"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("project map must hide ignored %q:\n%s", unwanted, got)
		}
	}
}

// TestRepoMapReachesAgentPrompts wires the ranked map into both loops that
// orient a fresh context: a direct turn and a leaf worker. The directory
// listing still exists, but the model now also knows which declarations the
// rest of the repository actually uses.
func TestRepoMapReachesAgentPrompts(t *testing.T) {
	root := t.TempDir()
	put := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	put("core/core.go", "package core\n\nfunc CoreThing() {}\n\nfunc unusedHelper() {}\n")
	put("app/main.go", "package app\n\nimport \"example/core\"\n\nfunc Run() { core.CoreThing(); core.CoreThing() }\n")

	cfg := directCfg(t, root, t.TempDir(), "wire it up")
	tree := engine.NewTaskTree("t-repomap", "root", "wire it up")
	o, err := New(cfg, tree, nil, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Close)

	prompt := NewDirect(o, nil).openingPrompt()
	if !strings.Contains(prompt, "Ranked symbol map") || !strings.Contains(prompt, "func CoreThing()") {
		t.Fatalf("direct prompt missing ranked repo map:\n%s", prompt)
	}
	if strings.Contains(prompt, "unusedHelper") {
		t.Fatalf("unreferenced unexported helper should not be advertised:\n%s", prompt)
	}

	node, ok := tree.CloneNode(tree.RootID)
	if !ok {
		t.Fatal("root node missing")
	}
	workerPrompt := o.projectContext(o.sandbox, node, "")
	if !strings.Contains(workerPrompt, "Ranked symbol map") || !strings.Contains(workerPrompt, "func CoreThing()") {
		t.Fatalf("worker context missing ranked repo map:\n%s", workerPrompt)
	}

	soloPrompt := o.soloTaskPrompt(o.sandbox, node)
	if !strings.Contains(soloPrompt, "Ranked symbol map") || !strings.Contains(soloPrompt, "func CoreThing()") {
		t.Fatalf("solo prompt missing ranked repo map:\n%s", soloPrompt)
	}
}
