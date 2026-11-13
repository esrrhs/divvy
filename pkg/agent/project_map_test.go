package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
