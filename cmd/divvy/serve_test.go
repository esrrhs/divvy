package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serveSentinelEnv switches the re-executed test binary into CLI mode so the
// process test can drive a real `divvy serve` (the bare test binary would
// otherwise reject -serve as an unknown test flag).
const serveSentinelEnv = "DIVVY_TEST_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(serveSentinelEnv) == "1" {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// In-process flag validation: serve is its own mode and cannot combine with
// other modes or a positional goal.
func TestRun_ServeMutex(t *testing.T) {
	cases := [][]string{
		{"-serve", "-guided"},
		{"-serve", "-interactive"},
		{"-serve", "-plan"},
		{"-serve", "-resume"},
		{"-serve", "-status"},
		{"-serve", "do something"},
	}
	for _, args := range cases {
		err := run(append([]string{"-datadir", t.TempDir(), "-workdir", t.TempDir()}, args...))
		if err == nil {
			t.Errorf("args %v: expected an error, got nil", args)
			continue
		}
		if !strings.Contains(err.Error(), "mutually exclusive") &&
			!strings.Contains(err.Error(), "no goal argument") {
			t.Errorf("args %v: expected a mutex/usage error, got %v", args, err)
		}
	}
}

// TR-6.1: the binary starts with -serve -port 0 -no-open, serves a
// token-protected health endpoint on loopback, and exits cleanly (exit 0)
// on SIGINT so running sessions can be checkpointed on shutdown.
func TestServe_ProcessHealthAndGracefulExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-driven shutdown test is POSIX-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work, data := t.TempDir(), t.TempDir()
	cmd := exec.Command(exe, "-serve", "-port", "0", "-no-open",
		"-workdir", work, "-datadir", data)
	cmd.Env = append(os.Environ(), serveSentinelEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Parse the token-bearing URL from startup output.
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if m := urlPattern.FindStringSubmatch(line); m != nil {
				select {
				case urlCh <- m[1]:
				default:
				}
			}
		}
	}()

	var accessURL string
	select {
	case accessURL = <-urlCh:
	case <-time.After(15 * time.Second):
		t.Fatal("serve never printed its access URL")
	}
	if !strings.HasPrefix(accessURL, "http://127.0.0.1:") || !strings.Contains(accessURL, "token=") {
		t.Fatalf("unexpected access URL: %s", accessURL)
	}

	// Token is required.
	resp, err := http.Get(strings.Split(accessURL, "?")[0] + "api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("health without token: %d, want 401", resp.StatusCode)
	}

	// With token → 200.
	resp, err = http.Get(strings.Split(accessURL, "?")[0] + "api/health?" + accessURL[strings.Index(accessURL, "?")+1:])
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("health with token: %d %s", resp.StatusCode, body)
	}

	// Root page is served without a token (placeholder shell).
	resp, err = http.Get(strings.Split(accessURL, "?")[0])
	if err != nil {
		t.Fatal(err)
	}
	rootBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rootBody), "divvy") {
		t.Fatalf("root page: %d", resp.StatusCode)
	}

	// SIGINT → exit code 0 within the graceful window.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exited non-zero after SIGINT: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("serve did not stop within 15s of SIGINT")
	}
}

var urlPattern = regexp.MustCompile(`(http://127\.0\.0\.1:\d+/\?token=[A-Za-z0-9_-]+)`)
