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
