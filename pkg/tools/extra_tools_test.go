package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The browser tests share one Chrome process: launching Chrome is expensive
// (and flaky on cold CI starts), so a single warm browser serves all tests.
var (
	sharedBrowserOnce sync.Once
	sharedBrowser     *BrowserClient
	sharedBrowserErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedBrowser != nil {
		sharedBrowser.Close()
	}
	os.Exit(code)
}

// TestBrowserClient returns a shared real headless browser; skipped if no
// Chrome. The browser launches lazily on the first action.
func testBrowserClient(t *testing.T) *BrowserClient {
	t.Helper()
	sharedBrowserOnce.Do(func() {
		sharedBrowser, sharedBrowserErr = NewBrowserClient()
	})
	if sharedBrowserErr != nil {
		t.Skipf("headless chrome unavailable: %v", sharedBrowserErr)
	}
	return sharedBrowser
}

func TestBrowser_NavigateJavaScript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>SPA Page</title></head>
<body><div id="app"></div>
<script>document.getElementById('app').textContent='rendered-by-js';</script>
</body></html>`))
	}))
	defer srv.Close()

	sb, _ := NewSandbox(t.TempDir())
	sb.Browser = testBrowserClient(t)

	out, err := sb.Call(context.Background(), ToolBrowserNavigate, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rendered-by-js") || !strings.Contains(out, "SPA Page") {
		t.Fatalf("JS content not rendered:\n%s", out)
	}
}

func TestBrowser_TypeClickFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>
<input id="field" type="text"/>
<button id="go" onclick="document.getElementById('out').textContent='hello-'+document.getElementById('field').value">Go</button>
<span id="out"></span>
</body></html>`))
	}))
	defer srv.Close()

	sb, _ := NewSandbox(t.TempDir())
	sb.Browser = testBrowserClient(t)
	ctx := context.Background()

	if _, err := sb.Call(ctx, ToolBrowserNavigate, map[string]any{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Call(ctx, ToolBrowserType, map[string]any{"selector": "#field", "text": "world"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Call(ctx, ToolBrowserClick, map[string]any{"selector": "#go"}); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(ctx, ToolBrowserText, map[string]any{"selector": "#out"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello-world") {
		t.Fatalf("click/type flow failed, got %q", out)
	}
}

func TestBrowser_ScreenshotSaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>shot target</h1></body></html>`))
	}))
	defer srv.Close()

	sb, _ := NewSandbox(t.TempDir())
	sb.Browser = testBrowserClient(t)
	ctx := context.Background()

	if _, err := sb.Call(ctx, ToolBrowserNavigate, map[string]any{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(ctx, ToolBrowserScreenshot, map[string]any{"path": "img/shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PNG") {
		t.Fatalf("expected PNG confirmation, got %q", out)
	}
	// PNG magic bytes.
	path := filepath.Join(sb.Root, "img/shot.png")
	data := readFile(t, path)
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatal("saved file is not a PNG")
	}
}

func TestBrowser_DisabledByDefault(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.Call(context.Background(), ToolBrowserNavigate, map[string]any{"url": "https://x"}); err == nil ||
		!strings.Contains(err.Error(), "-browser") {
		t.Fatalf("expected disabled error, got %v", err)
	}
}

func TestHTTP_RequestTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Header.Get("X-Test"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("got:" + r.Method))
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}

	out, err := sb.Call(context.Background(), ToolHTTPRequest, map[string]any{
		"url":     srv.URL + "/api",
		"method":  "POST",
		"headers": map[string]any{"X-Test": "abc"},
		"body":    "payload",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "201") || !strings.Contains(out, "X-Echo: abc") ||
		!strings.Contains(out, "got:POST") {
		t.Fatalf("unexpected http report:\n%s", out)
	}
}

func TestHTTP_RequestNeedsWeb(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	_, err := sb.Call(context.Background(), ToolHTTPRequest, map[string]any{"url": "https://x"})
	if err == nil || !strings.Contains(err.Error(), "-web") {
		t.Fatalf("expected -web gate error, got %v", err)
	}
}

func TestDownloadFile_Tool(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 0xff, 'h', 'i'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}

	out, err := sb.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url":  srv.URL + "/data.bin",
		"path": "assets/data.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "assets/data.bin") || !strings.Contains(out, "6 bytes") {
		t.Fatalf("unexpected report: %s", out)
	}
	if got := readFileBytes(t, filepath.Join(sb.Root, "assets/data.bin")); string(got) != string(payload) {
		t.Fatalf("binary content mismatch: %v", got)
	}

	// Offline sandbox refuses.
	plain, _ := NewSandbox(t.TempDir())
	if _, err := plain.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url": srv.URL, "path": "x",
	}); err == nil || !strings.Contains(err.Error(), "-web") {
		t.Fatalf("expected -web gate, got %v", err)
	}
	// Workspace escapes are rejected before any request.
	if _, err := sb.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url": srv.URL, "path": "../escape.bin",
	}); err == nil {
		t.Fatal("expected path escape error")
	}
}

func TestDownloadFile_ErrorCleansPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}
	_, err := sb.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url":  srv.URL + "/missing",
		"path": "out.bin",
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(sb.Root, "out.bin")); !os.IsNotExist(statErr) {
		t.Fatal("partial download must be removed")
	}
}

func TestDownloadFile_SizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One byte over the cap.
		_, _ = w.Write(make([]byte, maxDownloadBytes+1))
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}
	_, err := sb.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url":  srv.URL + "/big",
		"path": "big.bin",
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(sb.Root, "big.bin")); !os.IsNotExist(statErr) {
		t.Fatal("oversized partial download must be removed")
	}
}

// readFileBytes reads a fixture file, failing the test on error.
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGit_ReadTools(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()

	run := func(args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = sb.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := writeBinary(filepath.Join(sb.Root, "f.txt"), []byte("hi")); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "first commit")

	status, err := sb.Call(ctx, ToolGitStatus, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "##") {
		t.Fatalf("unexpected status:\n%s", status)
	}
	log, err := sb.Call(ctx, ToolGitLog, map[string]any{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "first commit") {
		t.Fatalf("unexpected log:\n%s", log)
	}
	diff, err := sb.Call(ctx, ToolGitDiff, map[string]any{"staged": false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "clean/empty") {
		t.Fatalf("expected clean diff, got:\n%s", diff)
	}
}

func TestGit_ToolsErrorOutsideRepo(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.Call(context.Background(), ToolGitStatus, nil); err == nil {
		t.Fatal("git tools must error outside a repository")
	}
}

func TestFindSymbol_GoAST(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("pkg/thing.go", `package pkg

type Widget struct{}

func BuildWidget() Widget { return Widget{} }
`); err != nil {
		t.Fatal(err)
	}

	out, err := sb.Call(context.Background(), ToolFindSymbol, map[string]any{"name": "BuildWidget", "kind": "func"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/thing.go:5 func BuildWidget") {
		t.Fatalf("AST location wrong:\n%s", out)
	}

	out, err = sb.Call(context.Background(), ToolFindSymbol, map[string]any{"name": "Widget", "kind": "type"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "type Widget") {
		t.Fatalf("type lookup failed:\n%s", out)
	}

	if out, _ = sb.Call(context.Background(), ToolFindSymbol, map[string]any{"name": "Missing"}); !strings.Contains(out, "no match") {
		t.Fatalf("expected no match, got %s", out)
	}
}

func TestFindSymbol_RegexFallback(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("a.py", "class Banana:\n    pass\n"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolFindSymbol, map[string]any{"name": "Banana"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.py:1") {
		t.Fatalf("regex fallback failed:\n%s", out)
	}
}

func TestJSONQuery_Tool(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("data.json", `{"items":[{"name":"first"},{"name":"second"}]}`); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"items.1.name":  "second",
		"items[0].name": "first",
		"items":         `[{"name":"first"},{"name":"second"}]`,
	}
	for q, want := range cases {
		out, err := sb.Call(context.Background(), ToolJSONQuery, map[string]any{"path": "data.json", "query": q})
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("query %q = %q, want %q", q, out, want)
		}
	}

	if _, err := sb.Call(context.Background(), ToolJSONQuery, map[string]any{"path": "data.json", "query": "missing.x"}); err == nil {
		t.Fatal("expected error for a missing path")
	}
}

func TestFindSymbol_References(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("pkg/a.go", "package pkg\n\nfunc BuildWidget() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("pkg/b.go", "package pkg\n\nfunc Use() {\n\tBuildWidget()\n\t// see BuildWidget for details\n}\n"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := sb.Call(ctx, ToolFindSymbol, map[string]any{"name": "BuildWidget", "references": true, "glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	// The call site (line 4) and the comment mention (line 5) are text-level
	// references...
	if !strings.Contains(out, "pkg/b.go:4:") || !strings.Contains(out, "BuildWidget()") {
		t.Fatalf("call site missing from references:\n%s", out)
	}
	if !strings.Contains(out, "pkg/b.go:5:") || !strings.Contains(out, "see BuildWidget") {
		t.Fatalf("comment mention is a valid text-level hit:\n%s", out)
	}
	// ...but the declaration line itself must be excluded.
	if strings.Contains(out, "pkg/a.go") {
		t.Fatalf("declaration must be excluded from references:\n%s", out)
	}

	// A name that is only declared has no references.
	out, err = sb.Call(ctx, ToolFindSymbol, map[string]any{"name": "BuildWidget", "references": true, "glob": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no references") {
		t.Fatalf("expected no-references result, got:\n%s", out)
	}

	// Word boundaries prevent BuildWidgetX from matching.
	if err := sb.WriteFile("pkg/c.go", "package pkg\nvar BuildWidgetX int\n"); err != nil {
		t.Fatal(err)
	}
	out, err = sb.Call(ctx, ToolFindSymbol, map[string]any{"name": "BuildWidget", "references": true, "glob": "c.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no references") && !strings.Contains(out, "no matches") {
		t.Fatalf("substring must not match, got:\n%s", out)
	}
}

// setupGitRepo creates a temp workspace initialized as a git repo with one
// baseline commit tracking the given files.
func setupGitRepo(t *testing.T, files map[string]string) *Sandbox {
	t.Helper()
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	run := func(args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = sb.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	for p, c := range files {
		if err := sb.WriteFile(p, c); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "baseline")
	return sb
}

func TestReviewDiff_Findings(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	dirty := `package main

func main() {
<<<<<<< HEAD
	fmt.Println("debug leftover")
	token := "abcdef1234567890"
=======
>>>>>>> branch
}
`
	if err := sb.WriteFile("main.go", dirty); err != nil {
		t.Fatal(err)
	}

	out, err := sb.Call(context.Background(), ToolReviewDiff, map[string]any{"staged": false})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[conflict] main.go:",
		"[debug] main.go:",
		"fmt.Println",
		"[secret] main.go:",
		"possible hard-coded secret",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in review:\n%s", want, out)
		}
	}
	if strings.Contains(out, "review_diff: clean") {
		t.Fatalf("dirty diff must not be clean:\n%s", out)
	}

	// Outside a repository the tool fails with a clear message.
	plain, _ := NewSandbox(t.TempDir())
	if _, err := plain.Call(context.Background(), ToolReviewDiff, nil); err == nil ||
		!strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("expected repo error, got %v", err)
	}
}

func TestReviewDiff_CleanAndEmpty(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	ctx := context.Background()

	// Nothing changed yet.
	out, err := sb.Call(ctx, ToolReviewDiff, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no tracked changes") {
		t.Fatalf("expected empty-diff message, got:\n%s", out)
	}

	// A benign modification passes review.
	if err := sb.WriteFile("main.go", "package main\n\n// harmless comment\n"); err != nil {
		t.Fatal(err)
	}
	out, err = sb.Call(ctx, ToolReviewDiff, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "review_diff: clean") {
		t.Fatalf("benign diff should be clean:\n%s", out)
	}
}

func TestReviewDiff_SizeWarnAndFindingCap(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{
		"main.go":  "package main\n",
		"extra.go": "package main\n",
	})

	// 650 changed lines in one tracked file trips the size warning.
	var big strings.Builder
	big.WriteString("package main\n")
	for i := 0; i < 650; i++ {
		big.WriteString("// padding line\n")
	}
	if err := sb.WriteFile("main.go", big.String()); err != nil {
		t.Fatal(err)
	}
	// Twelve debug statements exceed the 10-findings cap per kind.
	var dbg strings.Builder
	dbg.WriteString("package main\n")
	for i := 0; i < 12; i++ {
		dbg.WriteString("func dbg")
		dbg.WriteString(strconv.Itoa(i))
		dbg.WriteString("() { fmt.Println(1) }\n")
	}
	if err := sb.WriteFile("extra.go", dbg.String()); err != nil {
		t.Fatal(err)
	}

	out, err := sb.Call(context.Background(), ToolReviewDiff, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[size]") || !strings.Contains(out, "large diff") {
		t.Fatalf("expected size warning:\n%s", out)
	}
	if !strings.Contains(out, "2 more finding(s) omitted") {
		t.Fatalf("expected cap-omission note:\n%s", out)
	}
	if strings.Count(out, "[debug] extra.go:") != reviewMaxFindingsPerKind {
		t.Fatalf("expected exactly %d shown debug findings:\n%s", reviewMaxFindingsPerKind, out)
	}
}
