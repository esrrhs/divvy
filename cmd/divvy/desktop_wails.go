//go:build darwin

// Desktop shell: the same web UI the serve mode exposes, running in a native
// macOS window (WKWebView via Wails) instead of a browser tab.
//
// The window loads the UI from the wails:// scheme (static files served by
// the Wails asset server). Everything dynamic — /api calls and the SSE event
// stream — goes to the divvy server's own loopback listener via absolute
// URLs: macOS custom-scheme responses do not flush incrementally, so SSE
// must ride a real HTTP server to stream. The shell injects the backend port
// into the page and adds permissive CORS headers for it.
//
// No token is needed in desktop mode: the listener stays loopback-only and
// the desktop app runs as the user's own process (WithNoAuth).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/server"
	"github.com/esrrhs/divvy/web"
)

func desktopSupported() bool { return true }

func runDesktop(cfg agent.Config, log *agent.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := agent.NewRunManagerWithContext(ctx)
	srv, err := newServerForShell(cfg, log, mgr)
	if err != nil {
		return err
	}
	httpSrv, accessURL, err := srv.Start(0)
	if err != nil {
		return err
	}

	target, err := url.Parse(accessURL)
	if err != nil {
		return err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Flush immediately: the loopback leg is a real HTTP server, so SSE
	// streams all the way to the webview's fetch/EventSource.
	proxy.FlushInterval = -1
	proxy.Director = func(r *http.Request) {
		r.URL = target
		r.Host = target.Host
	}

	dist, distOK := web.DistFS()
	_ = distOK
	shell := &desktopShell{backendPort: target.Port(), proxy: proxy, dist: dist}

	// Wails lifecycle: closing the window stops the signal context, which
	// checkpoints every live run; Ctrl+C quits the window via the runtime.
	windowClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		wruntime.Quit(shell.appContext())
	}()

	log.Banner("divvy (desktop)")
	fmt.Printf("  workdir %s\n", cfg.WorkDir)
	fmt.Printf("  data    %s\n", cfg.DataDir)
	fmt.Printf("  backend %s (loopback; the window talks to it directly)\n", accessURL)

	err = wails.Run(&options.App{
		Title:     "divvy",
		Width:     1440,
		Height:    900,
		MinWidth:  960,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			// Static files (web/dist) served by the asset server itself;
			// /api + SSE hit the desktop middleware below. A Go-only build
			// without an embedded dist falls back to the server's built-in
			// placeholder page (served through the middleware).
			Handler: shell.middleware(),
		},
		OnStartup: func(c context.Context) { shell.setAppContext(c) },
		OnBeforeClose: func(context.Context) bool {
			stop()
			return false // allow the window to close; cleanup happens after Run
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
		},
	})
	close(windowClosed)
	<-windowClosed

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warnf("http shutdown: %v", err)
	}
	mgr.Shutdown(serveShutdownGrace)
	log.Infof("stopped. sessions are saved and resumable")
	return err
}

// desktopShell holds the desktop-mode plumbing: the Wails app context (for
// runtime calls), the backend proxy and port injected into the page, and the
// title of the conversation the window is currently showing.
type desktopShell struct {
	backendPort string
	proxy       http.Handler
	dist        fs.FS

	mu        sync.Mutex
	appCtx    context.Context
	lastTitle string
}

func (s *desktopShell) setAppContext(c context.Context) {
	s.mu.Lock()
	s.appCtx = c
	s.mu.Unlock()
}

func (s *desktopShell) appContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appCtx
}

var (
	convDetailRe = regexp.MustCompile(`^/api/conversations/[^/]+$`)
	assetsRe     = regexp.MustCompile(`^/(assets/|wails/|@vite)`)
	debugDesktop = os.Getenv("DIVVY_DESKTOP_DEBUG") != ""
)

// middleware wraps the backend proxy with the desktop-specific plumbing:
// CORS for the cross-origin window page, the backend-port injection on the
// initial HTML, OPTIONS preflight, and the window-title follower.
func (s *desktopShell) middleware() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if debugDesktop {
			fmt.Printf("[desktop] %s %s\n", r.Method, r.URL.Path)
		}
		if r.URL.Path == "/" {
			// The SPA entry: index.html with the backend port injected so
			// the frontend talks to the loopback server directly.
			s.serveIndexWithPort(rw, r)
			return
		}
		if assetsRe.MatchString(r.URL.Path) {
			// Static files: served from the embedded dist with the standard
			// file server (correct MIME), so module scripts load properly.
			http.StripPrefix("/", http.FileServer(http.FS(s.dist))).ServeHTTP(rw, r)
			return
		}
		// Everything below is cross-origin from wails://wails/.
		h := rw.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		if r.URL.Path == "/" {
			s.serveIndexWithPort(rw, r)
			return
		}

		if r.Method == http.MethodGet && convDetailRe.MatchString(r.URL.Path) {
			cap := &bodyCapture{ResponseWriter: rw}
			s.proxy.ServeHTTP(cap, r)
			var v struct {
				Title string `json:"title"`
			}
			if json.Unmarshal(cap.buf.Bytes(), &v) != nil || v.Title == "" {
				return
			}
			s.mu.Lock()
			changed := v.Title != s.lastTitle
			s.lastTitle = v.Title
			appCtx := s.appCtx
			s.mu.Unlock()
			if changed && appCtx != nil {
				// The window title follows the conversation: the native-app
				// equivalent of a window per document.
				wruntime.WindowSetTitle(appCtx, v.Title+" — divvy")
			}
			return
		}

		s.proxy.ServeHTTP(rw, r)
	})
}

// serveIndexWithPort serves the SPA's index.html with the backend port
// injected, so the frontend's fetch/EventSource calls go to the loopback
// server directly (streaming) instead of the buffering custom scheme.
func (s *desktopShell) serveIndexWithPort(rw http.ResponseWriter, r *http.Request) {
	rec := &bodyCapture{ResponseWriter: rw}
	s.proxy.ServeHTTP(rec, r)
	html := rec.buf.String()
	idx := strings.Index(html, "</head>")
	if idx < 0 {
		rw.Header().Set("Content-Length", fmt.Sprint(rec.buf.Len()))
		_, _ = rw.Write(rec.buf.Bytes())
		return
	}
	script := fmt.Sprintf(`<script>window.__DIVVY_BACKEND_PORT=%s</script>`, s.backendPort)
	out := html[:idx] + script + html[idx:]
	rw.Header().Set("Content-Length", fmt.Sprint(len(out)))
	_, _ = rw.Write([]byte(out))
}

// bodyCapture tees a response body so a wrapper can read what it forwarded.
type bodyCapture struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (w *bodyCapture) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return w.ResponseWriter.Write(p)
}

// newServerForShell builds the same server the serve mode runs, with auth
// off for the in-process webview.
func newServerForShell(cfg agent.Config, log *agent.Logger, mgr *agent.RunManager) (*server.Server, error) {
	opts := []server.Option{
		server.WithWorkdir(cfg.WorkDir),
		server.WithLogger(func(format string, args ...any) {
			log.Warnf(format, args...)
		}),
		server.WithNoAuth(),
	}
	if dist, ok := web.DistFS(); ok {
		opts = append(opts, server.WithStatic(dist))
	}
	return server.New(mgr, cfg.DataDir, opts...)
}
