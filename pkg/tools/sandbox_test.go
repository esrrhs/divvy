package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSandbox_PathEscapeAndCRUD(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sb.Resolve("../etc/passwd"); err == nil {
		t.Fatal("expected escape to fail")
	}

	if err := sb.WriteFile("pkg/a.go", "package pkg\n// line2\n// line3\n"); err != nil {
		t.Fatal(err)
	}
	got, err := sb.ReadFile("pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "package pkg") {
		t.Fatalf("read: %s", got)
	}

	if err := sb.ReplaceLines("pkg/a.go", 2, 2, "// replaced"); err != nil {
		t.Fatal(err)
	}
	got, _ = sb.ReadFile("pkg/a.go")
	if !strings.Contains(got, "// replaced") {
		t.Fatalf("replace lines failed: %s", got)
	}

	if err := sb.ReplaceText("pkg/a.go", "// replaced", "// once", false); err != nil {
		t.Fatal(err)
	}

	list, err := sb.ListDir(".", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "pkg/a.go") && !strings.Contains(list, filepath.Join("pkg", "a.go")) {
		t.Fatalf("list: %s", list)
	}

	res, err := sb.RunBash(context.Background(), "echo hi", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hi") {
		t.Fatalf("bash: %+v", res)
	}

	// A command that leaves a background child holding the inherited stdout
	// pipe blocks cmd.Wait (pipe EOF never arrives) until the timeout. The
	// whole process group must then be killed, promptly and without leaking
	// the child — this is the leaf-hang / server-leak guard.
	start := time.Now()
	res, err = sb.RunBash(context.Background(),
		"sleep 48 & wait", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatalf("timeout was not enforced promptly: %s", time.Since(start))
	}
	if leaked := pgrepSleep(48); leaked != "" {
		t.Fatalf("background child leaked after timeout: %s", leaked)
	}

	// Same guarantee when the child is orphaned in a subshell and the outer
	// shell itself exits immediately: pipe EOF still blocks to the timeout,
	// process-group cleanup kills the orphan.
	res, err = sb.RunBash(context.Background(),
		"(sleep 49 &)", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("orphaned child holding the pipe should also hit timeout: %+v", res)
	}
	if leaked := pgrepSleep(49); leaked != "" {
		t.Fatalf("orphaned child leaked: %s", leaked)
	}
}

// pgrepSleep reports surviving sleep processes by the distinctive argument.
// By the time this runs the parent sh is reaped, so a match means the sleep
// itself leaked; we avoid pgrep -x sleep which matches unrelated sleeps.
func pgrepSleep(seconds int) string {
	target := "sleep " + strconv.Itoa(seconds)
	for i := 0; i < 20; i++ {
		out, err := exec.Command("pgrep", "-f", target).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := exec.Command("pgrep", "-f", target).Output()
	return strings.TrimSpace(string(out))
}

func TestSandbox_ReadFileLines(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := "line one\nline two\nline three\nline four\n"
	if err := sb.WriteFile("f.txt", content); err != nil {
		t.Fatal(err)
	}

	// Inclusive 1-indexed range comes back numbered.
	got, err := sb.ReadFileLines("f.txt", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "2: line two") || !strings.Contains(got, "3: line three") {
		t.Fatalf("range read: %q", got)
	}
	if strings.Contains(got, "line one") || strings.Contains(got, "line four") {
		t.Fatalf("range leaked neighboring lines: %q", got)
	}

	// end=0 reads from start to EOF; end beyond the file is clamped.
	got, err = sb.ReadFileLines("f.txt", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "4: line four") || strings.Contains(got, "line three") {
		t.Fatalf("start-to-EOF read: %q", got)
	}
	got, _ = sb.ReadFileLines("f.txt", 4, 99)
	if !strings.Contains(got, "4: line four") {
		t.Fatalf("clamped end read: %q", got)
	}

	// Invalid ranges are errors.
	for _, tc := range [][2]int{{0, 1}, {-1, 2}, {3, 2}, {99, 100}} {
		if _, err := sb.ReadFileLines("f.txt", tc[0], tc[1]); err == nil {
			t.Fatalf("expected error for range %d-%d", tc[0], tc[1])
		}
	}

	// A range deep inside a file larger than the whole-read 64KB cap must
	// still be reachable — this is why ranged reads bypass ReadFile's cap.
	sb.MaxRead = 1024
	var big strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&big, "filler line %03d with padding padding padding\n", i)
	}
	big.WriteString("DEEP_TARGET_LINE\n")
	if err := sb.WriteFile("big.txt", big.String()); err != nil {
		t.Fatal(err)
	}
	got, err = sb.Call(context.Background(), ToolReadFile, map[string]any{
		"path":       "big.txt",
		"start_line": 301,
		"end_line":   301,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "301: DEEP_TARGET_LINE") {
		t.Fatalf("deep ranged read failed: %q", got)
	}
}

func TestSandbox_CallReadFileRange(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.txt", "a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}
	// No range arguments keeps the whole-file behavior.
	out, err := sb.Call(context.Background(), ToolReadFile, map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nb\nc\n" {
		t.Fatalf("whole-file read changed: %q", out)
	}
	// Ranged call via the tool dispatcher.
	out, err = sb.Call(context.Background(), ToolReadFile, map[string]any{
		"path": "a.txt", "start_line": 2, "end_line": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2: b") {
		t.Fatalf("ranged call: %q", out)
	}
	// Missing path is still an error.
	if _, err := sb.Call(context.Background(), ToolReadFile, map[string]any{}); err == nil {
		t.Fatal("expected missing-path error")
	}
}

func TestSandbox_ReadFilesBatch(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.txt", "aaa\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("dir/b.txt", "bbb\n"); err != nil {
		t.Fatal(err)
	}

	out, err := sb.Call(context.Background(), ToolReadFile, map[string]any{
		"paths": []any{"a.txt", "dir/b.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### a.txt", "aaa", "### dir/b.txt", "bbb"} {
		if !strings.Contains(out, want) {
			t.Fatalf("batch read missing %q:\n%s", want, out)
		}
	}
	// Header order follows the requested order.
	if strings.Index(out, "### a.txt") > strings.Index(out, "### dir/b.txt") {
		t.Fatalf("batch order not preserved:\n%s", out)
	}

	// One missing path fails the whole batch (no partial return).
	if _, err := sb.Call(context.Background(), ToolReadFile, map[string]any{
		"paths": []any{"a.txt", "missing.txt"},
	}); err == nil {
		t.Fatal("batch with a missing file must fail")
	}
	// Empty batch and over-cap batch are rejected.
	if _, err := sb.Call(context.Background(), ToolReadFile, map[string]any{
		"paths": []any{},
	}); err == nil {
		t.Fatal("empty batch must fail")
	}
	many := make([]any, maxBatchReadFiles+1)
	for i := range many {
		many[i] = "a.txt"
	}
	if _, err := sb.Call(context.Background(), ToolReadFile, map[string]any{
		"paths": many,
	}); err == nil {
		t.Fatalf("batch over %d files must fail", maxBatchReadFiles)
	}
}

func TestSandbox_CallWrite(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), "write_file", map[string]any{
		"path":    "b.txt",
		"content": "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.txt") {
		t.Fatalf("call out: %s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "b.txt"))
	if string(raw) != "hello" {
		t.Fatalf("file content %q", raw)
	}
}

func TestSandbox_ReplaceTexts(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	original := "alpha\nbeta\ngamma\n"
	if err := sb.WriteFile("f.txt", original); err != nil {
		t.Fatal(err)
	}

	// A batch applies several edits with one rewrite.
	err := sb.ReplaceTexts("f.txt", []TextEdit{
		{Old: "alpha", New: "ALPHA"},
		{Old: "beta", New: "BETA"},
		{Old: "gamma\n", New: "GAMMA\n"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "f.txt")); got != "ALPHA\nBETA\nGAMMA\n" {
		t.Fatalf("batch result: %q", got)
	}

	// A missing anchor in ANY edit must leave the file byte-identical.
	before := readFile(t, filepath.Join(sb.Root, "f.txt"))
	err = sb.ReplaceTexts("f.txt", []TextEdit{
		{Old: "ALPHA", New: "alpha"},
		{Old: "does-not-exist", New: "x"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "f.txt")); got != before {
		t.Fatalf("failed batch must not touch the file:\nbefore=%q\nafter =%q", before, got)
	}

	// An ambiguous anchor without replace_all is also a no-op atomic failure;
	// edits apply sequentially in memory, so an anchor introduced by edit 1
	// is visible to edit 2.
	if err := sb.WriteFile("dup.txt", "x x\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceTexts("dup.txt", []TextEdit{{Old: "x", New: "y"}}, false); err == nil ||
		!strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "dup.txt")); got != "x x\n" {
		t.Fatalf("ambiguous edit changed the file: %q", got)
	}
	if err := sb.ReplaceTexts("dup.txt", []TextEdit{{Old: "x", New: "y"}}, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "dup.txt")); got != "y y\n" {
		t.Fatalf("replace_all result: %q", got)
	}

	// Guard rails: empty batch and empty anchor.
	if err := sb.ReplaceTexts("f.txt", nil, false); err == nil {
		t.Fatal("empty batch should fail")
	}
	if err := sb.ReplaceTexts("f.txt", []TextEdit{{Old: "", New: "z"}}, false); err == nil {
		t.Fatal("empty old_string should fail")
	}
}

func TestSandbox_CallReplaceLinesEdits(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("f.txt", "one two\n"); err != nil {
		t.Fatal(err)
	}
	// The JSON-mode/native path delivers edits as []any of map[string]any.
	out, err := sb.Call(context.Background(), ToolReplaceLines, map[string]any{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"old_string": "one", "new_string": "1"},
			map[string]any{"old_string": "two", "new_string": "2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 edit(s)") {
		t.Fatalf("unexpected report: %s", out)
	}
	if got := readFile(t, filepath.Join(sb.Root, "f.txt")); got != "1 2\n" {
		t.Fatalf("edits call result: %q", got)
	}

	// Malformed shapes produce errors instead of panics.
	if _, err := sb.Call(context.Background(), ToolReplaceLines, map[string]any{
		"path":  "f.txt",
		"edits": "not-an-array",
	}); err == nil {
		t.Fatal("non-array edits should fail")
	}
	if _, err := sb.Call(context.Background(), ToolReplaceLines, map[string]any{
		"path":  "f.txt",
		"edits": []any{map[string]any{"new_string": "x"}},
	}); err == nil {
		t.Fatal("edit without old_string should fail")
	}
}

func TestSandbox_DeletePath(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("dir/b.txt", "y"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("dir/c.txt", "z"); err != nil {
		t.Fatal(err)
	}

	if _, err := sb.Call(context.Background(), ToolDeletePath, map[string]any{"path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("a.txt should be gone")
	}

	// A directory without recursive survives and returns a clear error.
	if err := sb.DeletePath("dir", false); err == nil ||
		!strings.Contains(err.Error(), "recursive") {
		t.Fatalf("expected recursive-required error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "dir", "b.txt")); err != nil {
		t.Fatalf("directory contents must survive a rejected delete: %v", err)
	}

	if _, err := sb.Call(context.Background(), ToolDeletePath, map[string]any{
		"path": "dir", "recursive": true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "dir")); !os.IsNotExist(err) {
		t.Fatal("dir should be gone")
	}

	// The workspace root itself is protected, and escapes stay rejected.
	if err := sb.DeletePath(".", true); err == nil {
		t.Fatal("deleting the workspace root must fail")
	}
	if err := sb.DeletePath("../escape", false); err == nil {
		t.Fatal("path escape must fail")
	}
}

func TestSandbox_MovePath(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("old.txt", "data"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pkg/sub/a.go", "package sub\n"); err != nil {
		t.Fatal(err)
	}

	// Move into a not-yet-existing directory creates the parents.
	if _, err := sb.Call(context.Background(), ToolMovePath, map[string]any{
		"from": "old.txt", "to": "new/dir/moved.txt",
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sb.Root, "new/dir/moved.txt")); got != "data" {
		t.Fatalf("moved content: %q", got)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("source should be gone")
	}

	// Moving a directory into its own subtree is rejected.
	if err := sb.MovePath("pkg", "pkg/inside"); err == nil {
		t.Fatal("move-into-self must fail")
	}
	// Same source/destination and missing source are errors.
	if err := sb.MovePath("new/dir/moved.txt", "new/dir/moved.txt"); err == nil {
		t.Fatal("same src/dst must fail")
	}
	if err := sb.MovePath("missing.txt", "x.txt"); err == nil {
		t.Fatal("missing source must fail")
	}

	// Replacing an existing file is allowed; an existing directory is not.
	if err := sb.WriteFile("victim.txt", "old"); err != nil {
		t.Fatal(err)
	}
	if err := sb.MovePath("new/dir/moved.txt", "victim.txt"); err != nil {
		t.Fatalf("file overwrite should succeed: %v", err)
	}
	if err := sb.MovePath("victim.txt", "pkg"); err == nil {
		t.Fatal("moving over an existing directory must fail")
	}
	if got := readFile(t, filepath.Join(sb.Root, "victim.txt")); got != "data" {
		t.Fatalf("overwrite result: %q", got)
	}
}
