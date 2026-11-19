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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

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

// desktopLockPort is the loopback port the FIRST desktop instance binds as
// its single-instance lock and focus-signaling endpoint. A second launch
// probes it: a live divvy shell answers, gets a focus request, and exits.
const desktopLockPort = 29571

func runDesktop(cfg agent.Config, log *agent.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Single instance: if another divvy desktop shell owns the lock port,
	// ask it to focus its window and exit quietly.
	if probeDesktopLock(desktopLockPort) {
		if focusErr := postDesktopFocus(desktopLockPort); focusErr != nil {
			return fmt.Errorf("已有 divvy 桌面窗口在运行（聚焦失败：%v）", focusErr)
		}
		fmt.Println("已有 divvy 桌面窗口在运行，已将其聚焦。")
		return nil
	}

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
	// Own the single-instance lock for this run: serve the ping/focus
	// endpoints on it for any later launch. A foreign process on the port
	// only costs us the lock — the app still runs.
	shell := &desktopShell{backendPort: target.Port(), proxy: proxy, dist: dist}
	shell.onFocus = func() {
		appCtx := shell.appContext()
		if appCtx == nil {
			return
		}
		// Surface the window: show/unminimise via the runtime, then flash
		// the title so the user notices which window it was.
		wruntime.WindowShow(appCtx)
		wruntime.WindowUnminimise(appCtx)
		wruntime.WindowSetTitle(appCtx, "divvy — 已聚焦")
		time.Sleep(1500 * time.Millisecond)
		shell.mu.Lock()
		title := "divvy"
		if shell.lastTitle != "" {
			title = shell.lastTitle + " — divvy"
		}
		shell.mu.Unlock()
		wruntime.WindowSetTitle(appCtx, title)
	}
	if lockLn, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", desktopLockPort)); lerr == nil {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/desktop/ping", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"app":"divvy","desktop":true}`)
			})
			mux.HandleFunc("POST /api/desktop/focus", func(w http.ResponseWriter, r *http.Request) {
				shell.fireFocus()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			})
			_ = http.Serve(lockLn, mux)
		}()
	} else {
		log.Warnf("desktop lock port busy (%v); starting without single-instance lock", lerr)
	}

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
	onFocus     func()

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

// probeDesktopLock reports whether another divvy desktop shell owns the
// lock port (identified by its ping marker, so a foreign process on the
// port does not count).
func probeDesktopLock(port int) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/desktop/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		App     string `json:"app"`
		Desktop bool   `json:"desktop"`
	}
	return json.NewDecoder(resp.Body).Decode(&v) == nil && v.App == "divvy" && v.Desktop
}

func postDesktopFocus(port int) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/api/desktop/focus", port), "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("focus endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// fireFocus runs the shell's focus action synchronously: the second
// instance's request returns after the window has actually been surfaced.
func (s *desktopShell) fireFocus() {
	if s.onFocus != nil {
		s.onFocus()
	}
}
