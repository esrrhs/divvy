package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/server"
	webui "github.com/esrrhs/divvy/web"
)

// serveShutdownGrace bounds both HTTP connection draining and the wait for
// in-flight runs to checkpoint after a signal.
const serveShutdownGrace = 10 * time.Second

// runServe starts the loopback web server, prints its token-bearing URL,
// optionally opens a browser, and blocks until SIGINT/SIGTERM. On shutdown
// the listener stops accepting and every live run is interrupted and
// checkpointed, so all sessions are resumable afterwards.
func runServe(cfg agent.Config, log *agent.Logger, port int, noOpen bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := agent.NewRunManagerWithContext(ctx)
	opts := []server.Option{
		server.WithWorkdir(cfg.WorkDir),
		server.WithLogger(func(format string, args ...any) {
			log.Warnf(format, args...)
		}),
	}
	// Serve the committed/built UI when it is embedded; otherwise New falls
	// back to its built-in placeholder page (Go-only development builds).
	if dist, ok := webui.DistFS(); ok {
		opts = append(opts, server.WithStatic(dist))
	}
	srv, err := server.New(mgr, cfg.DataDir, opts...)
	if err != nil {
		return err
	}

	httpSrv, accessURL, err := srv.Start(port)
	if err != nil {
		return err
	}

	log.Banner("divvy (serve)")
	fmt.Printf("  URL     %s\n", accessURL)
	fmt.Printf("  workdir %s\n", cfg.WorkDir)
	fmt.Printf("  data    %s\n", cfg.DataDir)
	fmt.Println("  press Ctrl+C to stop (running sessions are saved and resumable)")

	if !noOpen {
		if err := openBrowser(accessURL); err != nil {
			log.Warnf("could not open browser: %v (open the URL manually)", err)
		}
	}

	<-ctx.Done()
	log.Warnf("interrupt received; shutting down…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warnf("http shutdown: %v", err)
	}
	// Runs are already interrupted via the manager's signal-derived
	// context; wait for their checkpoints to land.
	mgr.Shutdown(serveShutdownGrace)
	log.Infof("stopped. resume sessions from the web UI or with: divvy -guided -resume -session <id>")
	return nil
}

// openBrowser launches the platform's default browser without attaching it
// to this process (the browser outlives divvy serve).
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{url}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		name = "xdg-open"
		args = []string{url}
	}
	return exec.Command(name, args...).Start()
}
