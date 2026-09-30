package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSearchFiles(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pkg/add.go", "package pkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pkg/add_test.go", "package pkg\n\nfunc TestAdd() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("notes.txt", "func not really code"); err != nil {
		t.Fatal(err)
	}

	// Basic regex search across the tree.
	out, err := sb.SearchFiles("func Add", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/add.go:3:") {
		t.Fatalf("missing file:line match:\n%s", out)
	}
	// notes.txt only contains "func not", must not match "func Add".
	if strings.Contains(out, "notes.txt") {
		t.Fatalf("unexpected match:\n%s", out)
	}

	// Glob filter restricts to test files / path.
	out, err = sb.SearchFiles("func", "", "pkg/*_test.go", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/add_test.go") || strings.Contains(out, "pkg/add.go:") {
		t.Fatalf("glob filter failed:\n%s", out)
	}

	// Basename glob "*.go" matches Go files in subdirectories.
	out, err = sb.SearchFiles("package", "", "*.go", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "notes.txt") || !strings.Contains(out, "pkg/add.go") {
		t.Fatalf("basename glob failed:\n%s", out)
	}

	// Case sensitivity: uppercase pattern, no default flag → no match;
	// with case_sensitive off (default) → matches via (?i).
	if out, _ := sb.SearchFiles("FUNC ADD", "", "*.go", false); !strings.Contains(out, "pkg/add.go") {
		t.Fatalf("case-insensitive search failed:\n%s", out)
	}
	if out, _ := sb.SearchFiles("FUNC ADD", "", "*.go", true); !strings.Contains(out, "no matches") {
		t.Fatalf("case-sensitive search should not match:\n%s", out)
	}

	// Search rooted in a subdirectory.
	out, err = sb.SearchFiles("TestAdd", "pkg", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/add_test.go:3:") {
		t.Fatalf("subdir search failed:\n%s", out)
	}

	// No matches message.
	if out, _ := sb.SearchFiles("zzz_nope", "", "", false); !strings.Contains(out, "no matches") {
		t.Fatalf("expected no-match message:\n%s", out)
	}

	// Invalid regex and empty pattern are errors.
	if _, err := sb.SearchFiles("[invalid", "", "", false); err == nil {
		t.Fatal("expected regex error")
	}
	if _, err := sb.SearchFiles("  ", "", "", false); err == nil {
		t.Fatal("expected empty-pattern error")
	}
}

func TestSearchFilesCall(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.go", "package a\nfunc F() {}\n"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolSearchFiles, map[string]any{
		"pattern": "func F",
		"glob":    "*.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2:") {
		t.Fatalf("call result:\n%s", out)
	}

	// Missing required pattern.
	if _, err := sb.Call(context.Background(), ToolSearchFiles, map[string]any{}); err == nil {
		t.Fatal("expected missing-pattern error")
	}
}
