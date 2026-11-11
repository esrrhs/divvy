//go:build linux

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isolateShellCommand wraps `sh -c command` in bubblewrap so the process
// can only write to the sandbox root (and ephemeral tmpfs). The rest of the
// host filesystem is mounted read-only or hidden, and a new network
// namespace blocks outbound access. If bwrap cannot be used, the command is
// not started — we never fall back to an unsandboxed shell.
func isolateShellCommand(ctx context.Context, root, command string, env []string) (*exec.Cmd, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("run_bash requires filesystem isolation (bubblewrap); bwrap is not available and the command was not executed")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("sandbox workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox workdir is not a directory")
	}

	args := []string{
		"--unshare-all",
		"--die-with-parent",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	}
	for _, p := range []string{"/usr", "/usr/local", "/etc", "/bin", "/lib", "/lib64", "/sbin", "/opt"} {
		args = appendRoBind(args, p)
	}
	args = appendDependencyCaches(args)
	// Bind the workspace after tmpfs mounts so a workdir under /tmp stays visible and writable.
	args = append(args, "--bind", abs, abs, "--chdir", abs, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.Dir = abs
	cmd.Env = isolatedEnv(env)
	return cmd, nil
}

func appendRoBind(args []string, path string) []string {
	info, err := os.Lstat(path)
	if err != nil {
		return args
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return args
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		// Recreate the host symlink rather than bind-mounting through it.
		return append(args, "--symlink", target, path)
	}
	if !info.IsDir() {
		return args
	}
	return append(args, "--ro-bind", path, path)
}

// isolatedEnvAllow is the only host environment names that may be
// inherited. Everything else (tokens, sentinels, user secrets, home
// paths) is dropped. Go/npm module caches are provided as read-only
// binds, not by exposing the host home directory.
var isolatedEnvAllow = map[string]bool{
	"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
	"LC_MESSAGES": true, "LC_COLLATE": true, "LC_NUMERIC": true,
	"LC_TIME": true, "LC_MONETARY": true, "TZ": true, "TERM": true,
}

func isolatedEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, 16)
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if isolatedEnvAllow[name] || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	out = append(out,
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"TMP=/tmp",
		"TEMP=/tmp",
		"GOCACHE=/tmp/divvy-gocache",
		"GOPATH=/tmp/divvy-gopath",
		"GOMODCACHE=/tmp/divvy-gomodcache",
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOSUMDB=off",
		"XDG_CACHE_HOME=/tmp/divvy-xdg-cache",
	)
	return out
}

func appendDependencyCaches(args []string) []string {
	if dir := hostGoModCache(); dir != "" {
		args = append(args, "--ro-bind", dir, "/tmp/divvy-gomodcache")
	}
	// /tmp is a fresh tmpfs per bwrap invocation. Bind a persistent host
	// build cache over GOCACHE so consecutive run_bash compiles in the
	// same conversation are not cold every time.
	if dir := hostGoBuildCache(); dir != "" {
		args = append(args, "--bind", dir, "/tmp/divvy-gocache")
	}
	return args
}

func hostGoBuildCache() string {
	dir := filepath.Join(os.TempDir(), "divvy-gocache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

func hostGoModCache() string {
	candidates := []string{strings.TrimSpace(os.Getenv("GOMODCACHE"))}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "go", "pkg", "mod"))
	}
	if p, err := exec.LookPath("go"); err == nil {
		_ = p
		if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
			candidates = append([]string{strings.TrimSpace(string(out))}, candidates...)
		}
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// ProbeShellIsolation reports whether run_bash can actually be isolated
// on this host. A missing or unusable bwrap is a hard unavailability,
// not an unsandboxed fallback.
func ProbeShellIsolation(root string) (bool, string) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false, "run_bash requires filesystem isolation (bubblewrap); bwrap is not available"
	}
	if strings.TrimSpace(root) == "" {
		root = os.TempDir()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, "run_bash isolation probe failed: " + err.Error()
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return false, "run_bash isolation probe failed: " + err.Error()
	}
	cmd, err := isolateShellCommand(context.Background(), abs, "true", isolatedEnv(nil))
	if err != nil {
		return false, err.Error()
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, "run_bash isolation is installed but cannot run here: " + msg
	}
	return true, ""
}
