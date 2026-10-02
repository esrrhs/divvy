package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const ddgSample = `<!doctype html><html><body>
<table>
<tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=x" class="result-link">The Go Programming Language</a></td></tr>
<tr><td class="result-snippet">Go is an open source programming language.</td></tr>
<tr><td><a rel="nofollow" href="https://blog.go.dev/" class="result-link">Go Blog</a></td></tr>
<tr><td class="result-snippet">News and release notes.</td></tr>
</table></body></html>`

func newTestWeb(t *testing.T, searchURL string, allowLocal bool) *WebClient {
	t.Helper()
	w, err := NewWebClient(searchURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	w.AllowLocal = allowLocal
	return w
}

func TestWeb_SearchDDGLite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ddgSample))
	}))
	defer srv.Close()

	w := newTestWeb(t, srv.URL+"/?q={query}", false)
	out, err := w.Search(context.Background(), "go language", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The Go Programming Language", "https://go.dev/", "open source", "Go Blog"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestWeb_SearchSearXNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"url":"https://example.com/a","title":"Example A","content":"snippet A"},
			{"url":"https://example.com/b","title":"Example B","content":"snippet B"}
		]}`))
	}))
	defer srv.Close()

	w := newTestWeb(t, srv.URL+"/search?q={query}&format=json", false)
	out, err := w.Search(context.Background(), "query", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Example A") || !strings.Contains(out, "https://example.com/a") {
		t.Fatalf("SearXNG parse failed:\n%s", out)
	}
	if strings.Contains(out, "Example B") {
		t.Fatal("max=1 should limit results")
	}
}

func TestWeb_SearchBadTemplate(t *testing.T) {
	if _, err := NewWebClient("https://example.com/search?q=x", time.Second); err == nil {
		t.Fatal("expected an error without the {query} placeholder")
	}
}

func TestWeb_FetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><style>.x{}</style></head>
<body><script>alert(1)</script><h1>Hello Web</h1><p>Readable&nbsp;paragraph.</p></body></html>`))
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	out, err := w.Fetch(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hello Web") || !strings.Contains(out, "Readable paragraph.") {
		t.Fatalf("HTML→text failed:\n%s", out)
	}
	if strings.Contains(out, "alert") || strings.Contains(out, ".x{}") {
		t.Fatal("script/style content must be removed")
	}
}

func TestWeb_FetchRejectsNonStandardPort(t *testing.T) {
	w := newTestWeb(t, "", false)
	u := &url.URL{Scheme: "http", Host: "example.com:8080", Path: "/"}
	if err := w.validateURL(u); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected port error, got %v", err)
	}
}

func TestWeb_FetchRejectsSchemeAndPrivate(t *testing.T) {
	w := newTestWeb(t, "", false)

	if err := w.validateURL(&url.URL{Scheme: "file", Path: "/etc/passwd"}); err == nil {
		t.Fatal("file scheme must be rejected")
	}

	// localhost resolves to a loopback address and must be blocked.
	if err := w.validateURL(&url.URL{Scheme: "http", Host: "localhost"}); err == nil {
		t.Fatal("loopback host must be blocked when AllowLocal is off")
	}

	// Allowed: same host passes when AllowLocal is on.
	w.AllowLocal = true
	if err := w.validateURL(&url.URL{Scheme: "http", Host: "localhost:9999"}); err != nil {
		t.Fatalf("AllowLocal should permit it: %v", err)
	}
}

func TestWeb_RedirectToLocalBlocked(t *testing.T) {
	var srv *http.Server
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("internal"))
	}))
	defer target.Close()

	srv = &http.Server{Addr: "127.0.0.1:0"}
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redir.Close()
	_ = srv

	w := newTestWeb(t, "", false)
	if _, err := w.Fetch(context.Background(), redir.URL); err == nil {
		t.Fatal("redirect to a loopback address must be rejected")
	}
}

func TestSandbox_WebToolsDisabledAndEnabled(t *testing.T) {
	sb, err := NewSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := sb.Call(ctx, ToolWebSearch, map[string]any{"query": "x"}); err == nil ||
		!strings.Contains(err.Error(), "web tools are disabled") {
		t.Fatalf("expected disabled error, got %v", err)
	}
	if _, err := sb.Call(ctx, ToolWebFetch, map[string]any{"url": "https://example.com"}); err == nil {
		t.Fatal("web_fetch must be disabled without -web")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ddgSample))
	}))
	defer srv.Close()

	sb.Web = newTestWeb(t, srv.URL+"/?q={query}", false)
	out, err := sb.Call(ctx, ToolWebSearch, map[string]any{"query": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "The Go Programming Language") {
		t.Fatalf("web_search via Call failed:\n%s", out)
	}
}
