package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func TestFindFiles(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"pkg/add.go", "pkg/add_test.go", "pkg/sub/helper_test.go",
		"cmd/main.go", "notes.txt", "node_modules/dep/index.js",
	} {
		if err := sb.WriteFile(p, "x"); err != nil {
			t.Fatal(err)
		}
	}

	// Bare basename glob matches at every directory depth.
	out, err := sb.FindFiles("*_test.go", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/add_test.go") || !strings.Contains(out, "pkg/sub/helper_test.go") {
		t.Fatalf("basename glob failed:\n%s", out)
	}
	if strings.Contains(out, "pkg/add.go") {
		t.Fatalf("non-test file leaked:\n%s", out)
	}

	// Basename *.go finds all Go files but never junk directories.
	out, _ = sb.FindFiles("*.go", "")
	if !strings.Contains(out, "cmd/main.go") || strings.Contains(out, "node_modules") {
		t.Fatalf("*.go matching wrong:\n%s", out)
	}

	// Slash-bearing glob matches the relative path only at that depth.
	out, _ = sb.FindFiles("pkg/*.go", "")
	if !strings.Contains(out, "pkg/add.go") || strings.Contains(out, "pkg/sub/helper_test.go") {
		t.Fatalf("path glob failed:\n%s", out)
	}

	// Search rooted in a subdirectory reports workspace-relative paths.
	out, _ = sb.FindFiles("*.go", "pkg/sub")
	if !strings.Contains(out, "pkg/sub/helper_test.go") || strings.Contains(out, "add.go\n") {
		t.Fatalf("rooted find failed:\n%s", out)
	}

	// No match, empty pattern, invalid glob.
	if out, _ := sb.FindFiles("*.zzz", ""); !strings.Contains(out, "no files matching") {
		t.Fatalf("expected no-match message:\n%s", out)
	}
	if _, err := sb.FindFiles("  ", ""); err == nil {
		t.Fatal("expected empty-pattern error")
	}
	if _, err := sb.FindFiles("[bad", ""); err == nil {
		t.Fatal("expected invalid-glob error")
	}
}

func TestFindFilesCall(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a_test.go", "x"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolFindFiles, map[string]any{"pattern": "*_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a_test.go") {
		t.Fatalf("call result:\n%s", out)
	}
	if _, err := sb.Call(context.Background(), ToolFindFiles, map[string]any{}); err == nil {
		t.Fatal("expected missing-pattern error")
	}
}

func TestSearchFilesHitsStructuredAndCaps(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pkg/add.go", "package pkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("notes.txt", "func not really code\n"); err != nil {
		t.Fatal(err)
	}
	// Ignored directory must not appear.
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "dep.js"), []byte("func Add hidden\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Binary / invalid UTF-8 is skipped.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0xff, 0xfe, 'A', 'd', 'd'}, 0644); err != nil {
		t.Fatal(err)
	}
	// Oversized file is skipped (limit is 1 MiB).
	big := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(big, append(bytes.Repeat([]byte("x"), maxSearchFileSize+8), []byte("func Add\n")...), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := sb.SearchFilesHits("func Add", "", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "pkg/add.go" || res.Hits[0].Line != 3 {
		t.Fatalf("hits=%+v", res.Hits)
	}
	if strings.Contains(res.Hits[0].Path, "/") && (filepath.IsAbs(res.Hits[0].Path) || strings.Contains(res.Hits[0].Path, dir)) {
		t.Fatalf("absolute or workdir path leaked: %q", res.Hits[0].Path)
	}
	for _, h := range res.Hits {
		if strings.Contains(h.Path, "node_modules") || strings.Contains(h.Path, "blob.bin") || h.Path == "huge.txt" {
			t.Fatalf("ignored/binary/oversized leak: %+v", h)
		}
	}

	// Result budget: stop after the cap and mark truncated.
	var many strings.Builder
	for i := 0; i < 30; i++ {
		many.WriteString("match-line\n")
	}
	if err := sb.WriteFile("many.txt", many.String()); err != nil {
		t.Fatal(err)
	}
	capped, err := sb.SearchFilesHits("match-line", "", "", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Hits) != 10 || !capped.Truncated {
		t.Fatalf("cap: n=%d truncated=%v", len(capped.Hits), capped.Truncated)
	}
}

func TestSearchFilesHitsSkipsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("func Add outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("ok.go", "package ok\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := sb.SearchFilesHits("func Add", "", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if strings.Contains(h.Path, "secret") || strings.Contains(h.Path, "link") || strings.Contains(h.Preview, "outside") {
			t.Fatalf("symlink escape leaked: %+v", h)
		}
		if filepath.IsAbs(h.Path) {
			t.Fatalf("absolute path: %q", h.Path)
		}
	}
}

func TestSearchFilesHitsSkipsNULBinary(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	// NUL is valid UTF-8; the file viewer rejects it as binary, so search must too.
	if err := os.WriteFile(filepath.Join(dir, "mixed.bin"), []byte("func Add\x00more text"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("ok.txt", "func Add is fine\n"); err != nil {
		t.Fatal(err)
	}
	res, err := sb.SearchFilesHits("func Add", "", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "ok.txt" {
		t.Fatalf("NUL binary leaked into hits: %+v", res.Hits)
	}
}

func TestSearchFilesHitsKeepsLongLineForLaterRedaction(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A 240-byte line must not be clipped inside SearchFilesHits; the HTTP
	// layer redacts the kept line first, then clips for display.
	long := strings.Repeat("x", 220) + "TARGET" + strings.Repeat("y", 20)
	if err := sb.WriteFile("long.txt", long+"\n"); err != nil {
		t.Fatal(err)
	}
	res, err := sb.SearchFilesHits("TARGET", "", "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits=%+v", res.Hits)
	}
	if !strings.Contains(res.Hits[0].Preview, "TARGET") {
		t.Fatalf("kept preview lost the match past 200 bytes: %q", res.Hits[0].Preview)
	}
	clipped := ClipSearchPreview(res.Hits[0].Preview)
	if strings.Contains(clipped, "TARGET") {
		t.Fatalf("display clip should not reach TARGET: %q", clipped)
	}
	if !strings.HasSuffix(clipped, "...") {
		t.Fatalf("expected clipped suffix, got %q", clipped)
	}
}
