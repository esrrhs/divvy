//go:build darwin

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireDarwinBackend skips the test when this host has no usable backend, so
// the suite still runs (and reports honestly) where sandbox-exec is missing or
// explicitly disabled.
func requireDarwinBackend(t *testing.T) *Sandbox {
	t.Helper()
	root := t.TempDir()
	if ok, reason := ProbeShellIsolation(root); !ok {
		t.Skipf("no execution backend here: %s", reason)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyRuntimePolicy(sb)
	if reason, off := sb.ToolDisabled(ToolRunBash); off {
		t.Skipf("run_bash disabled: %s", reason)
	}
	return sb
}

func runDarwin(t *testing.T, sb *Sandbox, cmd string) *ExecResult {
	t.Helper()
	res, err := sb.RunBash(context.Background(), cmd, 60*time.Second)
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return res
}

// TestDarwinBackend_RunsAndConfinesWrites is the core contract: commands run,
// the workspace is writable, and nothing else is.
func TestDarwinBackend_RunsAndConfinesWrites(t *testing.T) {
	sb := requireDarwinBackend(t)

	res := runDarwin(t, sb, "echo hello; printf x > inside.txt; cat inside.txt")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("sandboxed shell failed: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(sb.Root, "inside.txt")); err != nil {
		t.Fatalf("workspace write did not land: %v", err)
	}

	// A sibling of the workspace is outside the policy.
	outside := filepath.Join(filepath.Dir(sb.Root), "escape.txt")
	_ = os.Remove(outside)
	res = runDarwin(t, sb, "printf x > "+shellQuote(outside))
	if res.ExitCode == 0 {
		t.Fatalf("write outside the workspace succeeded: %+v", res)
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		t.Fatal("a file appeared outside the workspace")
	}
}

// TestDarwinBackend_HidesHomeSecrets: the read allowlist must not open the home
// directory, which is where credentials live.
func TestDarwinBackend_HidesHomeSecrets(t *testing.T) {
	sb := requireDarwinBackend(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	secret := filepath.Join(home, "divvy-test-secret-"+t.Name())
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Skipf("cannot write the sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	res := runDarwin(t, sb, "cat "+shellQuote(secret))
	if res.ExitCode == 0 || strings.Contains(res.Stdout+res.Stderr, "top-secret") {
		t.Fatalf("sandbox read a file in the home directory: %+v", res)
	}
	// The scratch HOME is what the toolchain sees instead.
	res = runDarwin(t, sb, "echo $HOME")
	if !strings.Contains(res.Stdout, "divvy-exec") {
		t.Fatalf("HOME should point at the scratch dir, got %q", res.Stdout)
	}
}

// TestDarwinBackend_NoNetwork keeps parity with the bubblewrap backend, which
// unshares the network namespace.
func TestDarwinBackend_NoNetwork(t *testing.T) {
	sb := requireDarwinBackend(t)
	res := runDarwin(t, sb, "curl -s -m 3 https://example.com >/dev/null 2>&1; echo rc=$?")
	if strings.Contains(res.Stdout, "rc=0") {
		t.Fatalf("network egress is not blocked: %+v", res)
	}
}

// TestDarwinBackend_GoToolchainWorks is the reason the allowlist exists: a Go
// edit has to be buildable and testable inside the sandbox, not merely parsable.
func TestDarwinBackend_GoToolchainWorks(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	sb := requireDarwinBackend(t)
	files := map[string]string{
		"go.mod": "module demo\n\ngo 1.21\n",
		"calc.go": `package demo

// Add returns a+b.
func Add(a, b int) int { return a + b }
`,
		"calc_test.go": `package demo

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3) = %d", got)
	}
}
`,
	}
	for name, body := range files {
		if err := sb.WriteFile(name, body); err != nil {
			t.Fatal(err)
		}
	}
	if res := runDarwin(t, sb, "go build -o /dev/null ./..."); res.ExitCode != 0 {
		t.Fatalf("go build inside the sandbox failed: %+v", res)
	}
	res := runDarwin(t, sb, "go test ./...")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "ok") {
		t.Fatalf("go test inside the sandbox failed: %+v", res)
	}
	// The build cache is redirected, so nothing is written to the host's cache
	// directory by the sandboxed process.
	if res := runDarwin(t, sb, "go env GOCACHE"); !strings.Contains(res.Stdout, "divvy-gocache") {
		t.Fatalf("GOCACHE should be redirected, got %q", res.Stdout)
	}
}

// TestDarwinBackend_DisabledByEnv pins the off switch: an operator who sets
// DIVVY_EXEC_BACKEND=none gets the unavailable error, never an unsandboxed run.
func TestDarwinBackend_DisabledByEnv(t *testing.T) {
	t.Setenv(execBackendEnv, "none")
	root := t.TempDir()
	if ok, reason := ProbeShellIsolation(root); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("probe should report the explicit disable, got ok=%v reason=%q", ok, reason)
	}
	sb, err := NewSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunBash(context.Background(), "echo hi", time.Second); err == nil {
		t.Fatal("run_bash must refuse to run when the backend is disabled")
	}
}

// TestDarwinProfile_Shape checks the policy text itself, because a profile that
// silently grew an "allow default" would pass every behaviour test above while
// isolating nothing.
func TestDarwinProfile_Shape(t *testing.T) {
	dirs, err := darwinExecDirs()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	profile := darwinProfile(root, dirs)

	musts := []string{
		"(version 1)",
		"(deny default)",
		"(deny network*)",
		`(deny mach-lookup (global-name "com.apple.securityd"))`,
		`(subpath "` + realPath(root) + `")`,
		`(subpath "` + dirs.tmp + `")`,
		`(subpath "` + dirs.cache + `")`,
	}
	for _, want := range musts {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile is missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "(allow default)") {
		t.Fatalf("profile must not allow everything by default:\n%s", profile)
	}
	// getcwd needs the ancestors, and they must stay literals: a subpath there
	// would expose every file next to the workspace.
	for _, anc := range parentDirs(realPath(root)) {
		if strings.Contains(profile, "(subpath "+sbplQuote(anc)+")") {
			t.Fatalf("ancestor %q must be a literal, not a subpath:\n%s", anc, profile)
		}
	}
	// The home directory may appear only as a literal (listing it), never as a
	// readable subpath.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if strings.Contains(profile, "(subpath "+sbplQuote(home)+")") {
			t.Fatalf("home must not be readable as a subpath:\n%s", profile)
		}
	}
}

// TestDarwinProfile_QuotesPaths keeps a directory with a quote in its name from
// producing a profile the sandbox rejects.
func TestDarwinProfile_QuotesPaths(t *testing.T) {
	dirs, err := darwinExecDirs()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), `we"ird dir`)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := darwinProfile(root, dirs)
	if !strings.Contains(profile, `we\"ird dir`) {
		t.Fatalf("quote was not escaped:\n%s", profile)
	}
	// A profile that does not compile is worse than no profile: it must run.
	if ok, reason := ProbeShellIsolation(root); !ok {
		t.Fatalf("profile with a quoted path did not run: %s", reason)
	}
}

// TestDarwinBackend_BuildsMainPackageInGitRepo covers the toolchain artifact
// the first config-cli bench run hit: `go build` stamps VCS state into a main
// package's binary, and git cannot run inside the sandbox (xcode-select is
// denied). Without -buildvcs=false the build fails on a workspace that is
// perfectly fine, and the model spends its steps chasing git and xcode.
func TestDarwinBackend_BuildsMainPackageInGitRepo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	sb := requireDarwinBackend(t)
	if err := sb.WriteFile("go.mod", "module demo\n\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("main.go", "package main\n\nfunc main() {}\n"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sb.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	res := runDarwin(t, sb, "go build -o /dev/null .")
	if res.ExitCode != 0 {
		t.Fatalf("building a main package in a git repo must not need VCS stamping: %+v", res)
	}
	if res := runDarwin(t, sb, "go env GOFLAGS"); !strings.Contains(res.Stdout, "buildvcs=false") {
		t.Fatalf("GOFLAGS should disable VCS stamping inside the sandbox, got %q", res.Stdout)
	}
}
