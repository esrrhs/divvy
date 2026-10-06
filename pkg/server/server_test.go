package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

func newTestServer(t *testing.T, opts ...Option) (*Server, *httptest.Server) {
	t.Helper()
	if len(opts) == 0 {
		opts = []Option{WithToken("test-token"), WithHeartbeat(50 * time.Millisecond)}
	}
	s, err := New(agent.NewRunManager(), t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, ts *httptest.Server, path string, modify func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if modify != nil {
		modify(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TR-4.1: token enforcement.
func TestAuth_TokenRequired(t *testing.T) {
	_, ts := newTestServer(t)

	resp := get(t, ts, "/api/health", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "token") {
		t.Fatalf("401 body should explain the token: %s", body)
	}

	resp = get(t, ts, "/api/health", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong-token")
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad bearer: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = get(t, ts, "/api/health?token=wrong", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad query token: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = get(t, ts, "/api/health", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer test-token")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer: status=%d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = get(t, ts, "/api/health?token=test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query token: status=%d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TR-4.1: Host allowlist (DNS-rebinding guard).
func TestAuth_HostAllowlist(t *testing.T) {
	_, ts := newTestServer(t)

	for _, host := range []string{"evil.example.com", "127.0.0.1.evil.com", "10.0.0.1", "example.com:8080"} {
		resp := get(t, ts, "/api/health?token=test-token", func(r *http.Request) {
			r.Host = host
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("host %q: status=%d, want 403", host, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Allowed loopback forms.
	for _, host := range []string{"localhost", "127.0.0.1", "127.0.0.1:8080", "[::1]:8080", "localhost:5173"} {
		resp := get(t, ts, "/api/health?token=test-token", func(r *http.Request) {
			r.Host = host
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("host %q: status=%d, want 200", host, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// Static assets load without a token; the placeholder page is served.
func TestStatic_PlaceholderWithoutToken(t *testing.T) {
	_, ts := newTestServer(t)
	resp := get(t, ts, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "divvy") {
		t.Fatalf("placeholder page missing branding:\n%s", body)
	}
	// Unknown SPA paths fall back to the shell.
	resp = get(t, ts, "/sessions/whatever", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SPA fallback: status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Start binds a real loopback listener and prints a token-bearing URL.
func TestStart_LoopbackAndURL(t *testing.T) {
	s, err := New(agent.NewRunManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, urlStr, err := s.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if !strings.HasPrefix(urlStr, "http://127.0.0.1:") || !strings.Contains(urlStr, "token="+s.Token()) {
		t.Fatalf("startup URL must be loopback with token: %s", urlStr)
	}
	resp, err := http.Get(strings.TrimSuffix(strings.Split(urlStr, "?")[0], "/") +
		"/api/health?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live health: %d", resp.StatusCode)
	}

	// The listener must really be loopback-bound.
	if !strings.HasPrefix(s.Addr(), "127.0.0.1:") {
		t.Fatalf("bound addr = %s, want 127.0.0.1", s.Addr())
	}
}
