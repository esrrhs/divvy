package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
