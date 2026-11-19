//go:build darwin

// Desktop shell: the same web UI the serve mode exposes, running in a native
// macOS window (WKWebView via Wails) instead of a browser tab. The divvy
// server still serves everything from a loopback listener on a random port;
// the Wails asset server forwards every window request to it (SSE included —
// the proxy flushes immediately so live phase updates stream through). No
// token is needed: the listener stays loopback-only and desktop mode runs as
// the user's own process.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
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
	// Flush immediately: the UI's SSE stream must not buffer inside the proxy.
	proxy.FlushInterval = -1
	// The webview's requests arrive with its custom-scheme host; send the
	// backend the loopback host or the host allowlist would reject them.
	proxy.Director = func(r *http.Request) {
		r.URL = target
		r.Host = target.Host
	}

	// Wails lifecycle: closing the window stops the signal context, which
	// checkpoints every live run; Ctrl+C quits the window via the runtime.
	var appCtx context.Context
	windowClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		wruntime.Quit(appCtx)
	}()

	log.Banner("divvy (desktop)")
	fmt.Printf("  workdir %s\n", cfg.WorkDir)
	fmt.Printf("  data    %s\n", cfg.DataDir)
	fmt.Printf("  backend %s (loopback, no browser)\n", accessURL)

	err = wails.Run(&options.App{
		Title:     "divvy",
		Width:     1440,
		Height:    900,
		MinWidth:  960,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			// Assets nil: every request (static + /api, GET and POST)
			// forwards to the divvy server.
			Handler: proxy,
		},
		OnStartup: func(c context.Context) { appCtx = c },
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
