// Package server hosts the local (loopback) divvy web API and SSE streams.
// It uses only the standard library: startup-token auth plus a Host
// allowlist defend against drive-by CSRF and DNS rebinding from another
// page open in the same browser.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/llm"
)

// defaultHeartbeat is the SSE keep-alive interval. Browsers/proxies often
// idle-drop silent streams around 30s; 15s stays comfortably below that.
const defaultHeartbeat = 15 * time.Second

// Server is the loopback HTTP surface for one RunManager.
type Server struct {
	mgr     *agent.RunManager
	dataDir string
	workdir string
	token   string
	static  fs.FS

	heartbeat time.Duration
	logf      func(format string, args ...any)
	addr      string

	// newClient builds the LLM client for a session. The default talks to
	// the configured OpenAI-compatible endpoint; tests inject scripted
	// clients through WithClientFactory.
	newClient func(cfg agent.Config) llm.Client
}

// Option customizes a Server (tests and the future -serve wiring use it).
type Option func(*Server)

// WithToken pins the startup token instead of generating one (tests).
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// WithHeartbeat overrides the SSE heartbeat interval (tests).
func WithHeartbeat(d time.Duration) Option {
	return func(s *Server) { s.heartbeat = d }
}

// WithStatic serves built frontend assets instead of the placeholder page.
func WithStatic(assets fs.FS) Option {
	return func(s *Server) { s.static = assets }
}

// WithLogger receives one-line operational logs; nil discards them.
func WithLogger(f func(format string, args ...any)) Option {
	return func(s *Server) { s.logf = f }
}

// WithWorkdir sets the default workspace for new sessions and the file API
// when a request does not name a session.
func WithWorkdir(dir string) Option {
	return func(s *Server) { s.workdir = dir }
}

// WithClientFactory overrides LLM client construction (tests use scripted
// clients instead of a real OpenAI-compatible endpoint).
func WithClientFactory(f func(cfg agent.Config) llm.Client) Option {
	return func(s *Server) { s.newClient = f }
}

// New creates a server over the run manager. dataDir is the sessions storage
// root used to replay/open sessions that are not currently live.
func New(mgr *agent.RunManager, dataDir string, opts ...Option) (*Server, error) {
	if mgr == nil {
		return nil, fmt.Errorf("server requires a run manager")
	}
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	s := &Server{
		mgr:       mgr,
		dataDir:   dataDir,
		workdir:   ".",
		token:     token,
		static:    defaultStaticFS(),
		heartbeat: defaultHeartbeat,
		logf:      func(string, ...any) {},
		newClient: newLLMClient,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Token returns the random URL token required by every /api request.
func (s *Server) Token() string { return s.token }

// generateToken mints 24 random bytes in URL-safe form (~32 chars): long
// enough that brute-forcing the loopback port is infeasible.
func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Handler builds the middleware-warmed router: Host allowlist first (a
// rebinding page must be rejected before anything else), then token auth on
// /api, then routing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions/{id}/log", s.handleSessionLog)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResumeSession)
	mux.HandleFunc("POST /api/sessions/{id}/plan/approve", s.handlePlanApprove)
	mux.HandleFunc("POST /api/sessions/{id}/plan/adjust", s.handlePlanAdjust)
	mux.HandleFunc("POST /api/sessions/{id}/abort", s.handleAbort)
	mux.HandleFunc("POST /api/sessions/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/sessions/{id}/redo", s.handleRedo)
	mux.HandleFunc("POST /api/sessions/{id}/add", s.handleAdd)
	mux.HandleFunc("POST /api/sessions/{id}/answer", s.handleAnswer)
	mux.HandleFunc("GET /api/sessions/{id}/approvals/{leafID}", s.handleGetApproval)
	mux.HandleFunc("POST /api/sessions/{id}/approvals/{leafID}", s.handleDecideApproval)
	mux.HandleFunc("GET /api/fs/list", s.handleFSList)
	mux.HandleFunc("GET /api/fs/file", s.handleFSFile)

	// Static frontend (or placeholder) for everything else, including SPA
	// client routes, with unknown paths falling back to index.html.
	mux.HandleFunc("/", s.handleStatic)

	return s.hostGuard(s.tokenGuard(mux))
}

// hostAllowlist is the DNS-rebinding defense: only loopback Host headers
// reach the app, so a malicious page that points its domain at 127.0.0.1
// cannot ride the browser's ambient request capabilities into the API.
func hostAllowlist(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowlist(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "forbidden host: this service only accepts loopback Host headers",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenGuard enforces the startup token on every /api request. Static assets
// stay reachable without it so the shell can load the page (the token rides
// in its first API calls instead).
func (s *Server) tokenGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="divvy"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "missing or invalid token",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	v, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"active_sessions": len(s.mgr.ActiveSessions()),
		"heartbeat_secs":  int(s.heartbeat.Seconds()),
	})
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if data, err := fs.ReadFile(s.static, name); err == nil {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(data)))
		return
	}
	// SPA fallback: unknown non-asset path renders the app shell.
	if data, err := fs.ReadFile(s.static, "index.html"); err == nil {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	http.NotFound(w, r)
}

// Start binds 127.0.0.1:port (0 = kernel-assigned) and serves in the
// background. The returned *http.Server is used by the caller for graceful
// Shutdown; the URL includes the token for one-click local access.
func (s *Server) Start(port int) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, "", fmt.Errorf("bind loopback: %w", err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.addr = ln.Addr().String()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logf("http serve: %v", err)
		}
	}()
	accessURL := "http://" + s.addr + "/?token=" + url.QueryEscape(s.token)
	return srv, accessURL, nil
}

// Addr reports the actual listener address ("127.0.0.1:port") once Start
// has bound it.
func (s *Server) Addr() string { return s.addr }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
