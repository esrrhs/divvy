package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepoFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestRepoMapRanksReferencedSymbols checks the core promise: the map lists
// declarations other files use, not whatever sorts first alphabetically, and
// it hides symbols that nothing references.
func TestRepoMapRanksReferencedSymbols(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "pkg/a/a.go", `package a

// Shared is used across packages.
func Shared() {}

// rareHelper is defined but never called.
func rareHelper() {}

type Widget struct{}
`)
	writeRepoFile(t, root, "pkg/b/b.go", `package b

import "example/pkg/a"

func Use() {
	a.Shared()
	a.Shared()
	var _ a.Widget
}
`)
	writeRepoFile(t, root, "pkg/c/c.go", `package c

import "example/pkg/a"

func UseAgain() { a.Shared() }
`)

	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := sb.RepoMap(4000)
	for _, want := range []string{"pkg/a/a.go", "func Shared()", "type Widget struct"} {
		if !strings.Contains(got, want) {
			t.Fatalf("repo map missing %q:\n%s", want, got)
		}
	}
	// Unreferenced unexported helpers must not crowd the map.
	if strings.Contains(got, "rareHelper") {
		t.Fatalf("unreferenced declaration should be omitted:\n%s", got)
	}
	if strings.Index(got, "func Shared()") > strings.Index(got, "type Widget struct") {
		t.Fatalf("more-referenced symbol should rank first:\n%s", got)
	}
}

// TestRepoMapRespectsIgnoreAndSkipsTests covers the retrieval boundary: a
// vendored or local-only tree must not dominate the ranking, and test helpers
// must not outrank production code.
func TestRepoMapRespectsIgnoreAndSkipsTests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGitTest(t, root, "init", "-q")
	writeRepoFile(t, root, ".gitignore", "vendor/\n")
	writeRepoFile(t, root, "app.go", `package app

func Entry() {}
`)
	writeRepoFile(t, root, "vendor/lib.go", `package vendor

func Entry() {}

func VendoredSecret() {}
`)
	// Many references to the ignored symbol: if ignore filtering is broken
	// it will rank above the project's own code.
	var uses strings.Builder
	uses.WriteString("package use\n\n")
	for i := 0; i < 5; i++ {
		uses.WriteString("func uses() { VendoredSecret(); VendoredSecret() }\n")
	}
	writeRepoFile(t, root, "uses.go", uses.String())
	writeRepoFile(t, root, "app_test.go", `package app

func TestHelper() { Entry(); Entry(); Entry() }
`)
	writeRepoFile(t, root, "cmd/main.go", `package main

import "example/app"

func main() { app.Entry() }
`)

	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	got := sb.RepoMap(4000)
	if strings.Contains(got, "vendor/lib.go") || strings.Contains(got, "VendoredSecret") {
		t.Fatalf("repo map must skip ignored paths:\n%s", got)
	}
	if strings.Contains(got, "app_test.go") || strings.Contains(got, "TestHelper") {
		t.Fatalf("repo map must skip test files:\n%s", got)
	}
	if !strings.Contains(got, "app.go") || !strings.Contains(got, "Entry") {
		t.Fatalf("repo map should include project code:\n%s", got)
	}
}

// TestRepoMapBudgetAndOtherLanguages keeps the map bounded and useful outside
// Go: the declaration patterns match JS/TS/Python/Rust signatures too.
func TestRepoMapBudgetAndOtherLanguages(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "web/app.ts", `export function renderPanel() {}

export class Panel {}
`)
	writeRepoFile(t, root, "web/use.ts", `import { renderPanel, Panel } from './app'

renderPanel()
const p: Panel | null = null
`)
	var many strings.Builder
	many.WriteString("package big\n\n")
	for i := 0; i < 200; i++ {
		many.WriteString("func Exported")
		many.WriteString(itoa(i))
		many.WriteString("() {}\n")
	}
	writeRepoFile(t, root, "big/big.go", many.String())

	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 600
	got := sb.RepoMap(budget)
	if len(got) > budget {
		t.Fatalf("repo map exceeded its budget: %d > %d", len(got), budget)
	}
	if !strings.Contains(got, "web/app.ts") || !strings.Contains(got, "renderPanel") {
		t.Fatalf("repo map should cover non-Go declarations:\n%s", got)
	}
}
