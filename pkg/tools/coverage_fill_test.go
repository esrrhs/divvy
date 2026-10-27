package tools

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// skipIfRoot guards the permission-dependent fixtures: chmod-based failure
// injection does not work for the superuser.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("chmod-based failure injection does not work as root")
	}
}

// occupyWithFile creates a regular file at path so that directory operations
// aimed at that path fail.
func occupyWithFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- sandbox.go edge branches ------------------------------------------------

func TestCovHiddenEnvRoundTrip(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if len(sb.HiddenEnv()) != 0 {
		t.Fatal("no keys hidden initially")
	}
	sb.HideEnv("OPENAI_API_KEY", "SECRET_TWO")
	got := sb.HiddenEnv()
	if len(got) != 2 || got[0] != "OPENAI_API_KEY" || got[1] != "SECRET_TWO" {
		t.Fatalf("HiddenEnv = %v", got)
	}
}

func TestCovNewSandboxBlockedPath(t *testing.T) {
	tmp := t.TempDir()
	occupyWithFile(t, filepath.Join(tmp, "occupied"))
	if _, err := NewSandbox(filepath.Join(tmp, "occupied", "sub")); err == nil {
		t.Fatal("expected MkdirAll failure")
	}
}

func TestCovRelNonAbsolutePath(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	// A relative path cannot be made relative to the absolute root; Rel must
	// return it unchanged instead of failing.
	if got := sb.Rel("plain.txt"); got != "plain.txt" {
		t.Fatalf("Rel = %q", got)
	}
}

func TestCovReadFilesErrors(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("ok.txt", "fine"); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(sb.Root, "locked.txt")
	occupyWithFile(t, occupied)
	if err := os.Chmod(occupied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(occupied, 0o644) })

	if _, err := sb.ReadFiles([]string{"../out.txt"}); err == nil {
		t.Fatal("escape in batch must fail")
	}
	if err := os.Mkdir(filepath.Join(sb.Root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFiles([]string{"adir"}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory in batch must fail, got %v", err)
	}
	if _, err := sb.ReadFiles([]string{"ok.txt", "locked.txt"}); err == nil {
		t.Fatal("unreadable member must fail the batch")
	}
}

func TestCovReadFileErrors(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.ReadFile("../out.txt"); err == nil {
		t.Fatal("escape must fail")
	}
	occupied := filepath.Join(sb.Root, "locked.txt")
	occupyWithFile(t, occupied)
	if err := os.Chmod(occupied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(occupied, 0o644) })
	if _, err := sb.ReadFile("locked.txt"); err == nil {
		t.Fatal("unreadable file must fail")
	}
}

func TestCovReadFileLinesEdges(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("text.txt", "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFileLines("../out.txt", 1, 2); err == nil {
		t.Fatal("escape must fail")
	}
	if _, err := sb.ReadFileLines("missing.txt", 1, 2); err == nil {
		t.Fatal("missing file must fail")
	}
	binary := filepath.Join(sb.Root, "blob.bin")
	if err := os.WriteFile(binary, []byte{'a', 0xff, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFileLines("blob.bin", 1, 1); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("binary content must fail, got %v", err)
	}
	// MaxRead unset falls back to the default instead of truncating instantly.
	sb.MaxRead = 0
	out, err := sb.ReadFileLines("text.txt", 1, 2)
	if err != nil || !strings.Contains(out, "two") {
		t.Fatalf("default MaxRead line read = %q %v", out, err)
	}
}

func TestCovWriteFileErrors(t *testing.T) {
	tmp := t.TempDir()
	sb, _ := NewSandbox(tmp)
	if err := sb.WriteFile("../out.txt", "x"); err == nil {
		t.Fatal("escape must fail")
	}
	occupyWithFile(t, filepath.Join(tmp, "occupied"))
	if err := sb.WriteFile("occupied/x.txt", "x"); err == nil {
		t.Fatal("MkdirAll over a file must fail")
	}
}

func TestCovReplaceEdges(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("f.txt", "a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fn   func() error
		want string
	}{
		{func() error { return sb.ReplaceLines("../x", 1, 2, "y") }, "escape"},
		{func() error { return sb.ReplaceText("../x", "a", "b", false) }, "escape"},
		{func() error { return sb.ReplaceTexts("../x", []TextEdit{{Old: "a", New: "b"}}, false) }, "escape"},
		{func() error { return sb.ReplaceTexts("missing.txt", []TextEdit{{Old: "a", New: "b"}}, false) }, "read"},
		{func() error { return sb.ReplaceText("f.txt", "zzz", "b", false) }, "not found"},
		{func() error { return sb.ReplaceLines("f.txt", 5, 1, "y") }, "range"},
	} {
		if err := tc.fn(); err == nil {
			t.Fatalf("%s: expected an error", tc.want)
		}
	}
	// end beyond EOF clamps to the last line.
	if err := sb.ReplaceLines("f.txt", 3, 99, "c2"); err != nil {
		t.Fatal(err)
	}
	got, err := sb.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "c2\n") {
		t.Fatalf("clamped replacement produced %q", got)
	}
}

func TestCovDeleteMoveErrors(t *testing.T) {
	tmp := t.TempDir()
	sb, _ := NewSandbox(tmp)
	if err := sb.WriteFile("a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := sb.DeletePath("missing.txt", false); err == nil {
		t.Fatal("missing path must fail")
	}
	if err := sb.MovePath("../out.txt", "b.txt"); err == nil {
		t.Fatal("escape source must fail")
	}
	if _, err := sb.ListDir("../out", false); err == nil {
		t.Fatal("escape listing must fail")
	}
	if _, err := sb.SearchFiles("p", "../out", "", false); err == nil {
		t.Fatal("escape search root must fail")
	}
	if _, err := sb.FindFiles("*", "../out"); err == nil {
		t.Fatal("escape find root must fail")
	}
	if err := sb.MovePath("a.txt", "../out.txt"); err == nil {
		t.Fatal("escape destination must fail")
	}
	if err := sb.MovePath("missing.txt", "b.txt"); err == nil {
		t.Fatal("missing source must fail")
	}
	occupyWithFile(t, filepath.Join(tmp, "occupied"))
	if err := sb.MovePath("a.txt", "occupied/b.txt"); err == nil {
		t.Fatal("MkdirAll over a file must fail")
	}
}

func TestCovListDirErrorsAndTruncation(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.ListDir("missing-dir", false); err == nil {
		t.Fatal("missing dir must fail")
	}
	locked := filepath.Join(sb.Root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := sb.ListDir("locked", false); err == nil {
		t.Fatal("unreadable dir must fail")
	}
	if _, err := sb.ListDir(".", true); err == nil {
		t.Fatal("recursive walk over unreadable dir must fail")
	}
	if snap := sb.Snapshot(); !strings.HasPrefix(snap, "(unable to list workspace") {
		t.Fatalf("Snapshot must report the failure, got %q", snap)
	}

	sb2, _ := NewSandbox(t.TempDir())
	sb2.MaxList = 2
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := sb2.WriteFile(n, "x"); err != nil {
			t.Fatal(err)
		}
	}
	out, err := sb2.ListDir(".", false)
	if err != nil || !strings.Contains(out, "[truncated to 2 entries]") {
		t.Fatalf("truncated listing = %q %v", out, err)
	}
	// A file carrying a skipped name is hidden from recursive listings too.
	if err := sb2.WriteFile(".git", "not a dir"); err != nil {
		t.Fatal(err)
	}
	rec, err := sb2.ListDir(".", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec, ".git") {
		t.Fatalf("skip-named file must be hidden:\n%s", rec)
	}
}

func TestCovRunBashTimeoutEmptyStderr(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	res, err := sb.RunBash(context.Background(), "sleep 5", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Stderr, "command timed out") {
		t.Fatalf("stderr must explain the timeout, got %q", res.Stderr)
	}
}

// --- call.go argument edge branches ------------------------------------------

func TestCovCallListDirDefaults(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("in.txt", "x"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := sb.Call(ctx, ToolListDir, map[string]any{})
	if err != nil || !strings.Contains(out, "in.txt") {
		t.Fatalf("default list_dir = %q %v", out, err)
	}
	if _, err := sb.Call(ctx, ToolListDir, map[string]any{"path": "no-such-dir"}); err == nil {
		t.Fatal("missing dir must fail")
	}
}

func TestCovCallWriteReplaceDeleteMoveErrors(t *testing.T) {
	tmp := t.TempDir()
	sb, _ := NewSandbox(tmp)
	occupyWithFile(t, filepath.Join(tmp, "occupied"))
	ctx := context.Background()
	cases := []struct {
		tool string
		args map[string]any
	}{
		{ToolWriteFile, map[string]any{}},
		{ToolWriteFile, map[string]any{"path": "x.txt"}},
		{ToolWriteFile, map[string]any{"path": "occupied/x.txt", "content": "x"}},
		{ToolReplaceLines, map[string]any{}},
		{ToolReplaceLines, map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "y"}},
		{ToolReplaceLines, map[string]any{"path": "f.txt", "start_line": 5, "end_line": 1}},
		{ToolDeletePath, map[string]any{"path": "missing.txt"}},
		{ToolMovePath, map[string]any{}},
		{ToolMovePath, map[string]any{"from": "missing.txt", "to": "b.txt"}},
		{ToolSQLiteQuery, map[string]any{}},
	}
	if err := sb.WriteFile("f.txt", "x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if _, err := sb.Call(ctx, c.tool, c.args); err == nil {
			t.Fatalf("%s %v: expected an error", c.tool, c.args)
		}
	}
}

func TestCovCallRunBashEmptyCommand(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.Call(context.Background(), ToolRunBash, map[string]any{"command": "   "}); err == nil {
		t.Fatal("blank command must fail")
	}
}

func TestCovCallWebToolArgErrors(t *testing.T) {
	tmp := t.TempDir()
	sb, _ := NewSandbox(tmp)
	sb.Web = newTestWeb(t, "", true)
	ctx := context.Background()
	if _, err := sb.Call(ctx, ToolHTTPRequest, map[string]any{}); err == nil {
		t.Fatal("http_request without url must fail")
	}
	if _, err := sb.Call(ctx, ToolDownloadFile, map[string]any{}); err == nil {
		t.Fatal("download_file without url must fail")
	}
	if _, err := sb.Call(ctx, ToolDownloadFile, map[string]any{"url": "http://example.com/x"}); err == nil {
		t.Fatal("download_file without path must fail")
	}
}

func TestCovCallSQLiteArgErrors(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	if _, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{"path": "d.db"}); err == nil {
		t.Fatal("sqlite_query without query must fail")
	}
}

// TestCovCallBrowserArgErrors exercises the browser tools' argument
// validation with a zero-value client: no Chrome process is involved because
// every case fails before the first CDP action.
func TestCovCallBrowserArgErrors(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sb.Browser = &BrowserClient{}
	defer sb.ReleaseBrowser()
	ctx := context.Background()
	cases := []struct {
		tool string
		args map[string]any
	}{
		{ToolBrowserNavigate, map[string]any{}},
		{ToolBrowserClick, map[string]any{}},
		{ToolBrowserType, map[string]any{}},
		{ToolBrowserScreenshot, map[string]any{}},
		{ToolBrowserScreenshot, map[string]any{"path": "../escape.png"}},
	}
	for _, c := range cases {
		if _, err := sb.Call(ctx, c.tool, c.args); err == nil {
			t.Fatalf("%s %v: expected an argument error", c.tool, c.args)
		}
	}
}

// TestCovBrowserLeaseCancel covers acquireBrowserLease surfacing a canceled
// context when another leaf holds the lease token.
func TestCovBrowserLeaseCancel(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	b := &BrowserClient{}
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	sb.Browser = b
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sb.Call(ctx, ToolBrowserNavigate, map[string]any{"url": "http://example.com"}); err == nil {
		t.Fatal("canceled acquire must fail")
	}
}

// --- symbols.go / search.go edge branches ------------------------------------

func TestCovFindSymbolEdges(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("code.go", "package p\n\nimport \"fmt\"\n\nvar CovVar = 1\n\nfunc MyFunc() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("node_modules/lib.js", "function MyFunc() {}"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("broken.go", "func ((("); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sb.Root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(sb.Root, "locked"), 0o755) })
	ctx := context.Background()

	if _, err := sb.FindSymbol(ctx, "  ", "", ""); err == nil {
		t.Fatal("blank name must fail")
	}
	// node_modules is skipped, code.go is filtered out by the glob, and the
	// regex fallback finds nothing under *.md either.
	out, err := sb.FindSymbol(ctx, "MyFunc", "", "*.md")
	if err != nil || !strings.HasPrefix(out, "no match") {
		t.Fatalf("glob-filtered search = %q %v", out, err)
	}
	out, err = sb.FindSymbol(ctx, "CovVar", "var", "")
	if err != nil || !strings.Contains(out, "var CovVar") {
		t.Fatalf("var lookup = %q %v", out, err)
	}
}

func TestCovFindReferencesEdges(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.FindReferences("  ", ""); err == nil {
		t.Fatal("blank name must fail")
	}
	var b strings.Builder
	for i := 0; i < 110; i++ {
		b.WriteString("use Widget here\n")
	}
	if err := sb.WriteFile("many.txt", b.String()); err != nil {
		t.Fatal(err)
	}
	out, err := sb.FindReferences("Widget", "many.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[more than 100 matches") {
		t.Fatalf("truncation note must pass through:\n%s", out)
	}
}

func TestCovSearchFilesEdges(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.SearchFiles("  ", "", "", false); err == nil {
		t.Fatal("blank pattern must fail")
	}
	if err := sb.WriteFile(".git/config", "needle"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("text.txt", "needle here\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.Root, "blob.bin"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.Root, "huge.txt"), bytes.Repeat([]byte("needle\n"), 200*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sb.Root, "text.txt"), filepath.Join(sb.Root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(sb.Root, "locked.txt")
	occupyWithFile(t, locked)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	out, err := sb.SearchFiles("needle", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "text.txt:1:") || strings.Contains(out, ".git") ||
		strings.Contains(out, "huge.txt") || strings.Contains(out, "locked.txt") {
		t.Fatalf("search respected skip/size/read/utf8 rules:\n%s", out)
	}
}

func TestCovFindFilesEdges(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.FindFiles("  ", ""); err == nil {
		t.Fatal("blank pattern must fail")
	}
	if err := sb.WriteFile(".git/config", "x"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("hit.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sb.Root, "hit.txt"), filepath.Join(sb.Root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sb.Root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(sb.Root, "locked"), 0o755) })
	out, err := sb.FindFiles("*.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hit.txt") || strings.Contains(out, ".git") ||
		strings.Contains(out, "locked") || strings.Contains(out, "link.txt") {
		t.Fatalf("find_files respected skip/regular rules:\n%s", out)
	}

	// More than 200 hits trigger the truncation note.
	for i := 0; i < 202; i++ {
		if err := sb.WriteFile(strconv.Itoa(i)+".dat", "x"); err != nil {
			t.Fatal(err)
		}
	}
	out, err = sb.FindFiles("*.dat", "")
	if err != nil || !strings.Contains(out, "[more than 200 files") {
		t.Fatalf("truncated find = %q %v", out, err)
	}
}

// --- sqlite / jsonquery / outline edge branches ------------------------------

func TestCovSQLiteQueryEdges(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.SQLiteQuery("d.db", "  ", 0); err == nil {
		t.Fatal("blank query must fail")
	}
	dbPath := filepath.Join(sb.Root, "d.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER, note TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES (1, 'x')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.SQLiteQuery("d.db", "SELECT * FROM missing_table", 0); err == nil {
		t.Fatal("broken SQL must fail")
	}
	// limit above the ceiling is clamped, not rejected.
	out, err := sb.SQLiteQuery("d.db", "SELECT id FROM t", 5000)
	if err != nil || !strings.Contains(out, "columns: id") {
		t.Fatalf("clamped query = %q %v", out, err)
	}
	// A []byte cell without a BLOB type name is rendered as text.
	if got := renderSQLiteValue([]byte("abc"), nil, 0); got != "abc" {
		t.Fatalf("renderSQLiteValue = %v", got)
	}
}

func TestCovJSONQueryEmptyPath(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.JSONQuery("", "a"); err == nil {
		t.Fatal("blank path must fail")
	}
	// A path that cannot be read surfaces the ReadFile error.
	if _, err := sb.JSONQuery("missing.json", "a"); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestCovOutlineEdges(t *testing.T) {
	skipIfRoot(t)
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.Outline("../x.go"); err == nil {
		t.Fatal("escape must fail")
	}
	if _, err := sb.Outline("."); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory must fail, got %v", err)
	}
	if err := sb.WriteFile("broken.go", "func ((("); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Outline("broken.go"); err == nil {
		t.Fatal("unparsable Go must fail")
	}
	if err := sb.WriteFile("plain.py", "def run():\n    pass\n"); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(sb.Root, "locked.py")
	occupyWithFile(t, locked)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := sb.Outline("locked.py"); err == nil {
		t.Fatal("unreadable non-Go file must fail")
	}
	// Oversized non-Go files are rejected rather than scanned. WriteFile
	// caps content, so write the oversized fixture directly.
	huge := strings.Repeat("# just a comment\n", 70*1024)
	if err := os.WriteFile(filepath.Join(sb.Root, "huge.py"), []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Outline("huge.py"); err == nil || !strings.Contains(err.Error(), "outline limit") {
		t.Fatalf("oversized file must fail, got %v", err)
	}

	// More than 200 declarations: the listing is capped.
	var b strings.Builder
	b.WriteString("package p\n")
	for i := 0; i < 205; i++ {
		b.WriteString("func F" + strconv.Itoa(i) + "() {}\n")
	}
	if err := sb.WriteFile("many.go", b.String()); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Outline("many.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != maxOutlineEntries {
		t.Fatalf("outline must cap at %d entries", maxOutlineEntries)
	}
}

// --- download.go edge branches -----------------------------------------------

func TestCovDownloadFileSandboxEdges(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	sb, _ := NewSandbox(tmp)
	if _, err := sb.DownloadFile(context.Background(), "http://example.com/x", "out.bin"); err == nil {
		t.Fatal("download without a WebClient must fail")
	}
	sb.Web = newTestWeb(t, "", true)
	occupyWithFile(t, filepath.Join(tmp, "occupied"))
	if _, err := sb.DownloadFile(context.Background(), "http://example.com/x", "occupied/out.bin"); err == nil {
		t.Fatal("MkdirAll over a file must fail")
	}
	ro := filepath.Join(tmp, "ro")
	if err := os.Mkdir(ro, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if _, err := sb.DownloadFile(context.Background(), "http://example.com/x", "ro/out.bin"); err == nil {
		t.Fatal("unwritable destination must fail")
	}
}

// --- git.go / gitread.go / gitreview.go edge branches ------------------------

func TestCovCommitAllAddError(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"a.txt": "one"})
	lockPath := filepath.Join(sb.Root, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })
	if _, _, err := CommitAll(sb.Root, "m"); err == nil || !strings.Contains(err.Error(), "git add") {
		t.Fatalf("locked index must fail git add, got %v", err)
	}
}

func TestCovCommitAllCommitError(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"a.txt": "one"})
	hook := filepath.Join(sb.Root, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("a.txt", "two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CommitAll(sb.Root, "m"); err == nil || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("failing hook must fail git commit, got %v", err)
	}
}

func TestCovGitReadErrors(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	if _, err := sb.GitStatus(ctx); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("non-repo status = %v", err)
	}
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.git(cctx, "status", "-sb"); err == nil {
		t.Fatal("canceled git command must fail")
	}
	if _, err := repo.git(ctx, "log", "--no-such-flag"); err == nil {
		t.Fatal("bad flag must fail")
	}
}

func TestCovReviewDiffStagedAndErrors(t *testing.T) {
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	if err := repo.WriteFile("a.txt", "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo.Root, "add", "-A")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	ctx := context.Background()
	out, err := repo.ReviewDiff(ctx, true)
	if err != nil || !strings.Contains(out, "review_diff:") {
		t.Fatalf("staged review = %q %v", out, err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.ReviewDiff(cctx, false); err == nil {
		t.Fatal("canceled review must fail")
	}
	if _, err := repo.PreFinishGate(cctx); err == nil {
		t.Fatal("canceled pre-finish gate must fail")
	}
}

func TestCovReviewResultHelpers(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("tracked.txt", "hello\n"); err != nil {
		t.Fatal(err)
	}
	r := newReviewResult()
	r.parseTrackedDiff(strings.Join([]string{
		"diff --git a/gone.txt b/gone.txt",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-deleted line",
	}, "\n"))
	if r.added != 0 || r.deleted != 1 {
		t.Fatalf("deleted-file diff must not add scan targets: %+v", r)
	}
	// Untracked scan skips blanks, escapes, directories and missing files;
	// only tracked.txt contributes.
	r2 := newReviewResult()
	r2.scanUntracked(sb, []string{"", "../out.txt", ".", "missing.txt", "tracked.txt"})
	if r2.added != 1 {
		t.Fatalf("only tracked.txt contributes: %+v", r2)
	}
	if got := clipReviewText(strings.Repeat("s", 120)); len(got) != 83 {
		t.Fatalf("clipReviewText = %d chars", len(got))
	}
}

// --- web.go edge branches ----------------------------------------------------

// redirectChainServer returns a server that 302s /hop/N to /hop/N+1 and
// answers 200 with "done" once n == stop.
func redirectChainServer(t *testing.T, stop int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n < stop {
			http.Redirect(w, r, "/hop/"+strconv.Itoa(n+1), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("done"))
	}))
}

func TestCovWebFetchTooManyRedirects(t *testing.T) {
	srv := redirectChainServer(t, 6) // never settles within 5 hops
	defer srv.Close()
	w := newTestWeb(t, "", true)
	if _, err := w.Fetch(context.Background(), srv.URL+"/hop/0"); err == nil ||
		!strings.Contains(err.Error(), "5 redirects") {
		t.Fatalf("six hops must trip the redirect guard, got %v", err)
	}
}

func TestCovWebFetchSingleRedirect(t *testing.T) {
	srv := redirectChainServer(t, 1)
	defer srv.Close()
	w := newTestWeb(t, "", true)
	out, err := w.Fetch(context.Background(), srv.URL+"/hop/0")
	if err != nil || out != "done" {
		t.Fatalf("redirected fetch = %q %v", out, err)
	}
}

func TestCovWebSearchRedirects(t *testing.T) {
	// A search endpoint that redirects once is followed; the landing page
	// carries a DDG-style result link.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/s") {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`<a href="https://a.example/" class="result-link">A</a><td class="result-snippet">s</td>`))
	}))
	defer srv.Close()
	w := newTestWeb(t, srv.URL+"/s?q={query}", true)
	out, err := w.Search(context.Background(), "0", 3)
	if err != nil || !strings.Contains(out, "https://a.example/") {
		t.Fatalf("redirected search = %q %v", out, err)
	}
	// One that never stops trips the search client's redirect guard.
	srv2 := redirectChainServer(t, 99)
	defer srv2.Close()
	w2 := newTestWeb(t, srv2.URL+"/hop/{query}", true)
	if _, err := w2.Search(context.Background(), "0", 3); err == nil ||
		!strings.Contains(err.Error(), "search request failed") {
		t.Fatalf("endless search redirect must fail, got %v", err)
	}
}

func TestCovWebFetchLoopbackLiteralBlocked(t *testing.T) {
	w := newTestWeb(t, "", false)
	// https keeps the standard port so the literal-IP policy is what rejects.
	if _, err := w.Fetch(context.Background(), "https://127.0.0.1/x"); err == nil ||
		!strings.Contains(err.Error(), "not public") {
		t.Fatalf("loopback literal must be rejected, got %v", err)
	}
}

func TestCovWebSearchEndpointFailures(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	w := newTestWeb(t, dead.URL+"/?q={query}", true)
	if _, err := w.Search(context.Background(), "x", 0); err == nil ||
		!strings.Contains(err.Error(), "search request failed") {
		t.Fatalf("dead search endpoint must fail, got %v", err)
	}
	// A SearXNG template with a non-JSON body surfaces the parse error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer bad.Close()
	w2 := newTestWeb(t, bad.URL+"/search?q={query}&format=json", true)
	if _, err := w2.Search(context.Background(), "x", 0); err == nil ||
		!strings.Contains(err.Error(), "SearXNG") {
		t.Fatalf("non-JSON SearXNG body must fail, got %v", err)
	}
	// A malformed search URL template fails at request construction.
	w3 := newTestWeb(t, "http://192.168.0.%zz?q={query}", true)
	if _, err := w3.Search(context.Background(), "x", 0); err == nil {
		t.Fatal("malformed search URL must fail")
	}
}

func TestCovWebRequestEdges(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	w := newTestWeb(t, "", true)
	ctx := context.Background()
	if _, err := w.Fetch(ctx, "http://[::1"); err == nil {
		t.Fatal("unparseable URL must fail")
	}
	if _, err := w.Fetch(ctx, "http:///no-host"); err == nil ||
		!strings.Contains(err.Error(), "missing a host") {
		t.Fatalf("hostless URL must fail, got %v", err)
	}
	if _, err := w.Fetch(ctx, deadURL+"/x"); err == nil {
		t.Fatal("dead endpoint must fail")
	}

	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("body"))
	}))
	defer text.Close()
	if _, err := w.Do(ctx, "", text.URL, nil, ""); err != nil {
		t.Fatalf("empty method defaults to GET: %v", err)
	}
	if _, err := w.Do(ctx, "BAD METHOD", text.URL, nil, ""); err == nil {
		t.Fatal("malformed method must fail")
	}
}

func TestCovWebReadBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare more bytes than are written so the client's read fails.
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("<p>short"))
	}))
	defer srv.Close()
	w := newTestWeb(t, "", true)
	if _, err := w.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("truncated body must fail the fetch")
	}
	if _, _, err := w.Download(context.Background(), srv.URL, io.Discard); err == nil {
		t.Fatal("truncated body must fail the download")
	}
}

func TestCovDownloadContentTypeVariants(t *testing.T) {
	binary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar; charset=binary")
		_, _ = w.Write([]byte("data"))
	}))
	defer binary.Close()
	w := newTestWeb(t, "", true)
	ct, n, err := w.Download(context.Background(), binary.URL, io.Discard)
	if err != nil || n != 4 || ct != "application/x-tar" {
		t.Fatalf("download = %q %d %v", ct, n, err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer empty.Close()
	ct, _, err = w.Download(context.Background(), empty.URL, io.Discard)
	if err != nil || ct != "application/octet-stream" {
		t.Fatalf("bodyless download = %q %v", ct, err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	if _, _, err := w.Download(context.Background(), dead.URL, io.Discard); err == nil {
		t.Fatal("dead endpoint must fail the download")
	}
}

func TestCovParseHelpers(t *testing.T) {
	res := parseDDGLite([]byte(`<a href="//example.com/x" class="result-link">Title</a>` +
		`<td class="result-snippet">A snippet</td>`))
	if len(res) != 1 || res[0][0] != "Title" || res[0][1] != "https://example.com/x" || res[0][2] != "A snippet" {
		t.Fatalf("parseDDGLite = %+v", res)
	}
	if _, err := parseSearXNG([]byte("not json")); err == nil {
		t.Fatal("invalid SearXNG JSON must fail")
	}
}

// --- mirror.go edge branches -------------------------------------------------

func TestCovNewMirrorErrors(t *testing.T) {
	skipIfRoot(t)
	// Missing source fails the copy walk.
	if _, err := NewMirror(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("missing source must fail")
	}
	// An unreadable file fails the copy.
	tmp := t.TempDir()
	locked := filepath.Join(tmp, "locked.txt")
	occupyWithFile(t, locked)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := NewMirror(tmp); err == nil {
		t.Fatal("unreadable file must fail the snapshot")
	}
}

func TestCovNewMirrorSkipsSymlinks(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "real.txt"), filepath.Join(tmp, "link.txt")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := os.Lstat(filepath.Join(m.Dir, "link.txt")); !os.IsNotExist(err) {
		t.Fatal("symlinks must not be copied into the mirror")
	}
}

func TestCovNewMirrorWorktreeAddError(t *testing.T) {
	skipIfRoot(t)
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	admin := filepath.Join(repo.Root, ".git", "worktrees")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(admin, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(admin, 0o755) })
	if _, err := NewMirror(repo.Root); err == nil {
		t.Fatal("worktree add must fail when the admin dir is unreadable")
	}
}

func TestCovWorktreeOverlayDiffError(t *testing.T) {
	skipIfRoot(t)
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	indexPath := filepath.Join(repo.Root, ".git", "index")
	good, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(indexPath, good, 0o644) })
	if err := os.WriteFile(indexPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMirror(repo.Root); err == nil {
		t.Fatal("a corrupt index must fail the worktree overlay")
	}
}

func TestCovWorktreeOverlayUntrackedEdges(t *testing.T) {
	skipIfRoot(t)
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	// A dangling symlink is skipped like any other non-regular file.
	if err := os.Symlink(filepath.Join(repo.Root, "nope.txt"), filepath.Join(repo.Root, "dangling")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := os.Lstat(filepath.Join(m.Dir, "dangling")); !os.IsNotExist(err) {
		t.Fatal("dangling symlink must not be overlaid")
	}

	// An unreadable untracked file fails the overlay.
	repo2 := setupGitRepo(t, map[string]string{"a.txt": "one"})
	locked := filepath.Join(repo2.Root, "locked.txt")
	occupyWithFile(t, locked)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := NewMirror(repo2.Root); err == nil {
		t.Fatal("unreadable untracked file must fail the overlay")
	}
}

// TestCovWorktreeOverlaySubdirs drives the worktree path with tracked changes
// in subdirectories: apply creates them and the digest pass walks them.
func TestCovWorktreeOverlaySubdirs(t *testing.T) {
	repo := setupGitRepo(t, map[string]string{
		"sub/a.txt":      "one",
		"dist/inner.txt": "generated",
	})
	if err := repo.WriteFile("sub/a.txt", "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteFile("dist/inner.txt", "generated v2"); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := os.ReadFile(filepath.Join(m.Dir, "sub", "a.txt"))
	if err != nil || string(got) != "one\ntwo\n" {
		t.Fatalf("worktree overlay missed the subdir change: %q %v", got, err)
	}
}

func TestCovClassifyWalkErrors(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	m.Close() // mirror directory gone
	if _, _, _, err := m.classifyChanges(); err == nil {
		t.Fatal("walking a vanished mirror must fail")
	}
	// A skipped-name directory inside the mirror is ignored.
	m2, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if err := os.MkdirAll(filepath.Join(m2.Dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m2.Dir, "node_modules", "junk.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes, removals, conflicts, err := m2.classifyChanges()
	if err != nil || len(writes) != 0 || len(removals) != 0 || len(conflicts) != 0 {
		t.Fatalf("ignored dirs must not be classified: %+v %+v %+v %v", writes, removals, conflicts, err)
	}
	// A non-regular entry is skipped too.
	if err := os.Symlink(filepath.Join(m2.Dir, "a.txt"), filepath.Join(m2.Dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = m2.classifyChanges(); err != nil {
		t.Fatalf("symlink in mirror must be skipped: %v", err)
	}
}

func TestCovMergeBackWriteErrors(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Unreadable mirror content fails the change classification.
	lockedMirror := filepath.Join(m.Dir, "a.txt")
	if err := os.Chmod(lockedMirror, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MergeBack(); err == nil {
		t.Fatal("unreadable mirror file must fail")
	}
	_ = os.Chmod(lockedMirror, 0o644)

	// Unreadable source content fails the classification too.
	lockedSrc := filepath.Join(tmp, "a.txt")
	if err := os.Chmod(lockedSrc, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedSrc, 0o644) })
	if _, _, err := m.MergeBack(); err == nil {
		t.Fatal("unreadable source file must fail")
	}
}

func TestCovMergeBackDeleteError(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.Remove(filepath.Join(m.Dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	// A source file that can no longer be read fails the deletion check.
	if err := os.Chmod(filepath.Join(tmp, "a.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MergeBack(); err == nil || !strings.Contains(err.Error(), "check a.txt") {
		t.Fatalf("unreadable source must fail the deletion check, got %v", err)
	}
	_ = os.Chmod(filepath.Join(tmp, "a.txt"), 0o644)

	// An undeletable source (read-only directory) fails the removal.
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })
	if _, _, err := m.MergeBack(); err == nil || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("undeletable source must fail, got %v", err)
	}
}

func TestCovPreviewChangesVariants(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	old := strings.Repeat("old line\n", 40*1024) // ~360KB
	if err := os.WriteFile(filepath.Join(tmp, "big.txt"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "bin.dat"), []byte("bin\x00ary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "plain.txt"), []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Modify everything in the mirror: big→truncated both sides, binary old,
	// plain→big new content.
	if err := os.WriteFile(filepath.Join(m.Dir, "big.txt"), []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir, "bin.dat"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir, "plain.txt"), bytes.Repeat([]byte("n"), 300*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := m.PreviewChanges()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileChange{}
	for _, fc := range changes {
		byPath[fc.Path] = fc
	}
	// Note: Truncated carries the NEW-content flag (overwritten below), so
	// the truncated OLD content is proven by its length instead.
	if fc := byPath["big.txt"]; fc.Status != ChangeModified || len(fc.OldContent) != previewMaxFileBytes {
		t.Fatalf("big.txt change = %+v", fc)
	}
	// The binary OLD content leaves OldContent empty (the flag itself is
	// later overwritten by the new-content preview, which is text).
	if fc := byPath["bin.dat"]; fc.OldContent != "" {
		t.Fatalf("bin.dat change = %+v", fc)
	}
	if fc := byPath["plain.txt"]; !fc.Truncated || fc.Binary || len(fc.NewContent) != previewMaxFileBytes {
		t.Fatalf("plain.txt change = %+v", fc)
	}

	// Sibling edits after the snapshot surface as conflicts, including new
	// files created on both sides.
	m2, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if err := os.WriteFile(filepath.Join(tmp, "plain.txt"), []byte("sibling edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "both.txt"), []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m2.Dir, "plain.txt"), []byte("leaf edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m2.Dir, "both.txt"), []byte("leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes2, err := m2.PreviewChanges()
	if err != nil {
		t.Fatal(err)
	}
	byPath2 := map[string]FileChange{}
	for _, fc := range changes2 {
		byPath2[fc.Path] = fc
	}
	if fc := byPath2["plain.txt"]; !fc.Conflict || fc.Status != ChangeModified {
		t.Fatalf("plain.txt conflict = %+v", fc)
	}
	if fc := byPath2["both.txt"]; !fc.Conflict || fc.Status != ChangeAdded {
		t.Fatalf("both.txt conflict = %+v", fc)
	}
	if _, _, err := m2.MergeBack(); err == nil {
		t.Fatal("MergeBack must refuse conflicting changes")
	}
}

func TestCovPreviewChangesClassifyError(t *testing.T) {
	skipIfRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.Chmod(filepath.Join(m.Dir, "a.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(m.Dir, "a.txt"), 0o644) })
	if _, err := m.PreviewChanges(); err == nil {
		t.Fatal("unreadable mirror content must fail the preview")
	}
}

func TestCovMirrorCloseEdgeCases(t *testing.T) {
	var nilMirror *Mirror
	nilMirror.Close() // must not panic
	empty := &Mirror{}
	empty.Close()

	// A worktree mirror whose source lost .git still deletes its directory.
	repo := setupGitRepo(t, map[string]string{"a.txt": "one"})
	m, err := NewMirror(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo.Root, ".git"), filepath.Join(repo.Root, ".git-moved")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(filepath.Join(repo.Root, ".git-moved"), filepath.Join(repo.Root, ".git")) })
	m.Close()
	if _, err := os.Stat(m.Dir); !os.IsNotExist(err) {
		t.Fatal("the worktree directory must be removed even when git refuses")
	}
}

func TestCovFileDigestDirect(t *testing.T) {
	if sum, err := fileDigest(filepath.Join(t.TempDir(), "missing")); err != nil || sum != "" {
		t.Fatalf("missing file digest = %q %v", sum, err)
	}
	// A directory opens but cannot be read.
	if _, err := fileDigest(t.TempDir()); err == nil {
		t.Fatal("directory digest must fail")
	}
	skipIfRoot(t)
	locked := filepath.Join(t.TempDir(), "locked.txt")
	occupyWithFile(t, locked)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := fileDigest(locked); err == nil {
		t.Fatal("unreadable file must fail the digest")
	}
}
