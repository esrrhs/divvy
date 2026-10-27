package tools

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandbox_RelSnapshotAndSkipNames(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("dir/a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if got := sb.Rel(filepath.Join(sb.Root, "dir", "a.txt")); got != filepath.Join("dir", "a.txt") {
		t.Fatalf("Rel = %q", got)
	}
	snap := sb.Snapshot()
	if !strings.Contains(snap, "a.txt") || !strings.Contains(snap, "dir/") {
		t.Fatalf("snapshot missing entries:\n%s", snap)
	}
	if !IsSkippedName(".git") || IsSkippedName("main.go") {
		t.Fatal("IsSkippedName disagrees with the skip table")
	}
}

func TestSandbox_NewSandboxEmptyRoot(t *testing.T) {
	sb, err := NewSandbox("")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if sb.Root != cwd {
		t.Fatalf("empty root should resolve to cwd: %q vs %q", sb.Root, cwd)
	}
}

func TestSandbox_ResolveBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if got, err := sb.Resolve(""); err != nil || got != sb.Root {
		t.Fatalf("empty path resolves to root, got %q %v", got, err)
	}
	if _, err := sb.Resolve("a/b.txt"); err != nil {
		t.Fatalf("relative path must resolve: %v", err)
	}
	if _, err := sb.Resolve(filepath.Join(sb.Root, "in.txt")); err != nil {
		t.Fatalf("absolute path inside root must resolve: %v", err)
	}
	if _, err := sb.Resolve("/etc/passwd"); err == nil {
		t.Fatal("absolute path outside root must be rejected")
	}
	if _, err := sb.Resolve("../out.txt"); err == nil {
		t.Fatal("relative escape must be rejected")
	}
}

func TestSandbox_ListDirBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	for _, p := range []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt", "sub/x.txt", ".git/config"} {
		if err := sb.WriteFile(p, "x"); err != nil {
			t.Fatal(err)
		}
	}

	out, err := sb.ListDir(".", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "f1.txt") || !strings.Contains(out, "sub/") {
		t.Fatalf("non-recursive listing needs dir suffix:\n%s", out)
	}
	if strings.Contains(out, "config") {
		t.Fatalf(".git must be skipped:\n%s", out)
	}

	out, err = sb.ListDir("sub", false)
	if err != nil || !strings.Contains(out, "x.txt") {
		t.Fatalf("sub listing = %q %v", out, err)
	}
	out, err = sb.ListDir("f1.txt", false)
	if err != nil || out != "f1.txt" {
		t.Fatalf("file path returns the relative name: %q %v", out, err)
	}
	if _, err := sb.ListDir("missing", false); err == nil {
		t.Fatal("missing path must error")
	}

	empty, _ := NewSandbox(t.TempDir())
	out, err = empty.ListDir(".", true)
	if err != nil || out != "(empty)" {
		t.Fatalf("empty workspace = %q %v", out, err)
	}

	sb.MaxList = 3
	out, err = sb.ListDir(".", true)
	if err != nil || !strings.Contains(out, "[truncated to 3 entries]") {
		t.Fatalf("truncated listing = %q %v", out, err)
	}
	sb.MaxList = 0 // falls back to the default cap
	if _, err = sb.ListDir(".", true); err != nil {
		t.Fatal(err)
	}
}

func TestSandbox_ReadFileBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("big.txt", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	sb.MaxRead = 10
	out, err := sb.ReadFile("big.txt")
	if err != nil || !strings.Contains(out, "[truncated to 10 bytes]") || len(out) != 10+len("\n\n[truncated to 10 bytes]") {
		t.Fatalf("truncated read = %q %v", out, err)
	}
	sb.MaxRead = 0 // default cap; content fits
	if out, err = sb.ReadFile("big.txt"); err != nil || len(out) != 40 {
		t.Fatalf("default cap read = %q %v", out, err)
	}
	// WriteFile does not validate encoding; plant invalid UTF-8 directly.
	if werr := os.WriteFile(filepath.Join(sb.Root, "bad.txt"), []byte{0xff, 0xfe, 'x'}, 0644); werr != nil {
		t.Fatal(werr)
	}
	if _, err := sb.ReadFile("bad.txt"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 must error, got %v", err)
	}
	if _, err := sb.ReadFile("."); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory read must error, got %v", err)
	}
	if _, err := sb.ReadFile("nope.txt"); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestSandbox_ReadFileLinesBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("f.txt", "l1\nl2\nl3\n"); err != nil {
		t.Fatal(err)
	}
	ctxErrs := []struct{ start, end int }{{0, 2}, {2, 1}}
	for _, r := range ctxErrs {
		if _, err := sb.ReadFileLines("f.txt", r.start, r.end); err == nil {
			t.Fatalf("range %d-%d must error", r.start, r.end)
		}
	}
	if _, err := sb.ReadFileLines("f.txt", 99, 0); err == nil ||
		!strings.Contains(err.Error(), "beyond file") {
		t.Fatal("start beyond EOF must error")
	}
	// end beyond EOF clamps; start without end reads to EOF.
	out, err := sb.ReadFileLines("f.txt", 2, 99)
	if err != nil || !strings.Contains(out, "3: l3") {
		t.Fatalf("clamped end = %q %v", out, err)
	}
	out, err = sb.ReadFileLines("f.txt", 2, 0)
	if err != nil || !strings.Contains(out, "2: l2") || !strings.Contains(out, "3: l3") {
		t.Fatalf("open-ended range = %q %v", out, err)
	}
	// Output cap truncates mid-window.
	sb.MaxRead = 8
	out, err = sb.ReadFileLines("f.txt", 1, 3)
	if err != nil || !strings.Contains(out, "[truncated to 8 bytes") {
		t.Fatalf("capped window = %q %v", out, err)
	}
	// A file over the line-read pre-read limit is refused with sed advice.
	huge := maxLineReadBytes + 1
	if werr := os.WriteFile(filepath.Join(sb.Root, "huge.txt"), bytes.Repeat([]byte("a"), huge), 0644); werr != nil {
		t.Fatal(werr)
	}
	if _, err := sb.ReadFileLines("huge.txt", 1, 1); err == nil ||
		!strings.Contains(err.Error(), "line-read limit") {
		t.Fatalf("oversized file must error, got %v", err)
	}
}

func TestSandbox_WriteFileBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sb.MaxWrite = 4
	if err := sb.WriteFile("big.txt", "12345"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over-cap write must error, got %v", err)
	}
	sb.MaxWrite = 0 // default cap
	if err := sb.WriteFile("nested/dir/f.txt", "ok"); err != nil {
		t.Fatalf("nested write failed: %v", err)
	}
	if content := readFile(t, sb.Root+"/nested/dir/f.txt"); content != "ok" {
		t.Fatalf("nested write content = %q", content)
	}
}

func TestSandbox_ReplaceLinesBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	// CRLF files keep their line endings.
	if err := os.WriteFile(filepath.Join(sb.Root, "crlf.txt"), []byte("a\r\nb\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceLines("crlf.txt", 1, 1, "A"); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, sb.Root+"/crlf.txt")
	if !strings.Contains(data, "A\r\nb") {
		t.Fatalf("CRLF endings lost: %q", data)
	}
	// Content with CRLF is normalized into the file's ending style.
	if err := sb.ReplaceLines("crlf.txt", 1, 2, "X\r\nY"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, sb.Root+"/crlf.txt"), "\r\r") {
		t.Fatal("replacement CRLF must be normalized")
	}
	for _, r := range [][2]int{{0, 1}, {2, 1}, {99, 100}} {
		if err := sb.ReplaceLines("crlf.txt", r[0], r[1], "z"); err == nil {
			t.Fatalf("invalid range %d-%d must error", r[0], r[1])
		}
	}
	if err := sb.ReplaceLines("nope.txt", 1, 1, "z"); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestSandbox_ReplaceTextBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("f.txt", "aaa bbb aaa\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceText("f.txt", "", "x", false); err == nil {
		t.Fatal("empty old_string must error")
	}
	if err := sb.ReplaceText("f.txt", "zzz", "x", false); err == nil {
		t.Fatal("missing old_string must error")
	}
	if err := sb.ReplaceText("f.txt", "aaa", "x", false); err == nil {
		t.Fatal("ambiguous old_string must error")
	}
	if err := sb.ReplaceText("f.txt", "aaa", "x", true); err != nil {
		t.Fatal(err)
	}
	if content := readFile(t, sb.Root+"/f.txt"); content != "x bbb x\n" {
		t.Fatalf("replace_all result = %q", content)
	}
	if err := sb.ReplaceText("f.txt", "bbb", "y", false); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceText("missing.txt", "a", "b", false); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestSandbox_ReplaceTextsAtomicity(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("f.txt", "one two\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.ReplaceTexts("f.txt", nil, false); err == nil {
		t.Fatal("empty edit list must error")
	}
	if err := sb.ReplaceTexts("f.txt", []TextEdit{{Old: "one", New: "1"}, {Old: "nope", New: "x"}}, false); err == nil {
		t.Fatal("batch with a missing anchor must fail")
	}
	if content := readFile(t, sb.Root+"/f.txt"); content != "one two\n" {
		t.Fatalf("failed batch must not half-apply, file is %q", content)
	}
	if err := sb.ReplaceTexts("f.txt", []TextEdit{{Old: "", New: "x"}}, false); err == nil {
		t.Fatal("empty old_string in a batch must error")
	}
}

func TestSandbox_MovePathBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sb.Root, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{{".", "b"}, {"a.txt", "a.txt"}, {"missing.txt", "b"}, {"a.txt", "d"}, {"d", "d/sub"}} {
		if err := sb.MovePath(m[0], m[1]); err == nil {
			t.Fatalf("move %v must error", m)
		}
	}
	if err := sb.MovePath("a.txt", "d/renamed.txt"); err != nil {
		t.Fatalf("move into existing dir must work: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "d/renamed.txt")); err != nil {
		t.Fatal("moved file missing")
	}
}

func TestSandbox_RunBashBranches(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.RunBash(context.Background(), "   ", 0); err == nil {
		t.Fatal("empty command must error")
	}
	// Timeout kills the shell and reports the reason.
	res, err := sb.RunBash(context.Background(), "sleep 5", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 || !strings.Contains(res.Stderr, "timed out") {
		t.Fatalf("timeout result = %+v", res)
	}
	// Hidden env vars never reach the subprocess.
	t.Setenv("DIVVY_TEST_SECRET", "leaky")
	sb.HideEnv("DIVVY_TEST_SECRET")
	res, err = sb.RunBash(context.Background(), "printf \"$DIVVY_TEST_SECRET\"", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "leaky") {
		t.Fatalf("hidden env leaked: %q", res.Stdout)
	}
	// Output over MaxOutput is truncated in place.
	sb.MaxOutput = 10
	res, err = sb.RunBash(context.Background(), "printf 'aaaaaaaaaaaaaaaaaaaaaaaaaa'", 0)
	if err != nil || !strings.Contains(res.Stdout, "[output truncated]") {
		t.Fatalf("truncation marker missing: %+v %v", res, err)
	}
	// A failed command surfaces its exit code instead of a Go error.
	res, err = sb.RunBash(context.Background(), "exit 7", 0)
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("exit code report = %+v %v", res, err)
	}
}

func TestCapWriterBehavior(t *testing.T) {
	// Partial write: "more" only fits three bytes, then the marker appears.
	var buf bytes.Buffer
	c := capWriter{w: &buf, n: 8}
	if n, err := c.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("under-cap write = %d, %v", n, err)
	}
	if n, _ := c.Write([]byte("more")); n != 4 {
		t.Fatalf("over-cap write must report full length, got %d", n)
	}
	if buf.String() != "hellomor\n[output truncated]" {
		t.Fatalf("cap content = %q", buf.String())
	}
	// Fully exhausted buffer silently swallows further writes.
	var full bytes.Buffer
	exhausted := capWriter{w: &full, n: 5}
	if n, err := exhausted.Write([]byte("exact")); err != nil || n != 5 {
		t.Fatalf("exact-fit write = %d, %v", n, err)
	}
	if n, _ := exhausted.Write([]byte("more")); n != 4 {
		t.Fatalf("exhausted write must report full length, got %d", n)
	}
	if full.String() != "exact" {
		t.Fatalf("exhausted content = %q", full.String())
	}
}

func TestWebClient_NewDefaultsAndValidation(t *testing.T) {
	w, err := NewWebClient("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.searchURL != defaultSearchURL {
		t.Fatalf("empty template should default to DDG lite, got %q", w.searchURL)
	}
	if _, err := NewWebClient("https://x.example/missing-placeholder", time.Second); err == nil {
		t.Fatal("template without {query} must error")
	}
	// Invalid dial address (no port) errors before any dial.
	if _, err := w.secureDialContext(context.Background(), "tcp", "example.com"); err == nil ||
		!strings.Contains(err.Error(), "invalid dial address") {
		t.Fatalf("portless dial address must error, got %v", err)
	}
}

func TestWebClient_ResolvePublicIPs(t *testing.T) {
	w, _ := NewWebClient("", time.Second)
	w.AllowLocal = false
	if _, err := w.resolvePublicIPs(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("loopback literal must be rejected")
	}
	ips, err := w.resolvePublicIPs(context.Background(), "8.8.8.8")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Fatalf("public literal should pass: %v %v", ips, err)
	}
	if _, err := w.resolvePublicIPs(context.Background(), "no-such-host.invalid"); err == nil {
		t.Fatal("unresolvable host must error")
	}
}

func TestWebClient_FetchUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x00, 0x01})
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	if _, err := w.Fetch(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("binary fetch must error, got %v", err)
	}
}

func TestWebClient_DownloadErrors(t *testing.T) {
	w := newTestWeb(t, "", true)
	if _, _, err := w.Download(context.Background(), "://bad url", &bytes.Buffer{}); err == nil {
		t.Fatal("unparseable URL must error")
	}
	if _, _, err := w.Download(context.Background(), "file:///etc/hosts", &bytes.Buffer{}); err == nil {
		t.Fatal("non-http scheme must error")
	}

	// A 3xx hop to a disallowed scheme is stopped by the redirect policy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, _, err := w.Download(context.Background(), srv.URL, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("404 download must error, got %v", err)
	}
}

func TestWebClient_SearchEmptyAndNoResults(t *testing.T) {
	w := newTestWeb(t, "", true)
	if _, err := w.Search(context.Background(), "  ", 0); err == nil {
		t.Fatal("empty query must error")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>nothing useful</body></html>"))
	}))
	defer srv.Close()
	wn := newTestWeb(t, srv.URL+"/?q={query}", true)
	out, err := wn.Search(context.Background(), "query", 0)
	if err != nil || !strings.Contains(out, "no results") {
		t.Fatalf("no-results message = %q %v", out, err)
	}
}

func TestBrowser_LeaseAndCloseEdgeCases(t *testing.T) {
	// Release before any Acquire (nil lease) is a no-op.
	zero := &BrowserClient{}
	zero.Release()

	// Close before the browser starts: only the allocator is canceled.
	fresh, err := NewBrowserClient()
	if err != nil {
		t.Skipf("headless chrome unavailable: %v", err)
	}
	fresh.Close()
	// Close again after ensure created the browser context.
	fresh2, _ := NewBrowserClient()
	if err := fresh2.ensure(); err != nil {
		t.Fatal(err)
	}
	fresh2.Close()
}

func TestBrowser_ActionErrorsAfterContextCancel(t *testing.T) {
	b, err := NewBrowserClient()
	if err != nil {
		t.Skipf("headless chrome unavailable: %v", err)
	}
	if err := b.ensure(); err != nil {
		t.Fatal(err)
	}
	b.allocCancel()

	ctx := context.Background()
	if _, err := b.Navigate(ctx, "https://example.invalid/"); err == nil {
		t.Fatal("Navigate must fail after allocator cancel")
	}
	if err := b.Click(ctx, "#x"); err == nil {
		t.Fatal("Click must fail after allocator cancel")
	}
	if err := b.Type(ctx, "#x", "text", true); err == nil {
		t.Fatal("Type must fail after allocator cancel")
	}
	if _, err := b.Text(ctx, "#x"); err == nil {
		t.Fatal("Text must fail after allocator cancel")
	}
	if _, err := b.Screenshot(ctx); err == nil {
		t.Fatal("Screenshot must fail after allocator cancel")
	}
	b.Close()
}
