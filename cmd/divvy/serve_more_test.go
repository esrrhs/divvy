package main

import (
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

// TestRunServe_ShutdownOnSignal starts the web UI in-process, waits for the
// token URL, then delivers SIGINT to prove the graceful shutdown path runs
// (listener drained, runs interrupted, nil error).
func TestRunServe_ShutdownOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-driven shutdown test is POSIX-only")
	}
	cfg := agent.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()

	// Capture runServe's stdout (it prints the token-bearing URL).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- runServe(cfg, agent.SilentLogger(), 0, true) }()

	// Wait for the access URL (the signal handler is registered long before
	// it), then interrupt the serve loop.
	var banner strings.Builder
	buf := make([]byte, 2048)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := w.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err == nil {
			if n, _ := r.Read(buf); n > 0 {
				banner.Write(buf[:n])
			}
		}
		if strings.Contains(banner.String(), "http://127.0.0.1:") {
			break
		}
	}
	_ = w.SetReadDeadline(time.Time{})
	if !strings.Contains(banner.String(), "http://127.0.0.1:") {
		os.Stdout = orig
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-done
		t.Fatalf("serve never printed its access URL, got:\n%s", banner.String())
	}
	if !strings.Contains(banner.String(), "token=") {
		os.Stdout = orig
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-done
		t.Fatalf("access URL must carry the token:\n%s", banner.String())
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	runErr := <-done
	os.Stdout = orig
	w.Close()

	if runErr != nil {
		t.Fatalf("runServe must shut down cleanly: %v", runErr)
	}
	out := banner.String()
	if !strings.Contains(out, "press Ctrl+C") || !strings.Contains(out, "workdir") {
		t.Fatalf("startup banner incomplete:\n%s", out)
	}
}

// TestRunServe_PortTakenFails covers the listener error branch: an occupied
// port must surface as an error from runServe.
func TestRunServe_PortTakenFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-driven test setup is POSIX-only")
	}
	// Grab a live listener so Start cannot bind it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	cfg := agent.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DataDir = t.TempDir()

	done := make(chan error, 1)
	go func() { done <- runServe(cfg, agent.SilentLogger(), port, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("binding an occupied port must fail")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServe did not return on a taken port")
		// Cleanup: the serve loop is still blocked on the signal; interrupt it.
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-done
	}
}
