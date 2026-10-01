package tools

import (
	"context"
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
