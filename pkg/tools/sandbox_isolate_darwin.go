//go:build darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Darwin execution backend
//
// The Linux backend wraps commands in bubblewrap: the host filesystem is hidden
// except system paths, the workspace is the only writable tree, and the network
// namespace is empty. macOS has no bubblewrap, but it ships the same seatbelt
// kernel sandbox Xcode uses, driven by /usr/bin/sandbox-exec. This file builds
// an equivalent policy for it so run_bash is not simply unavailable off Linux.
//
// What the profile enforces (verified by ProbeShellIsolation at startup, not
// merely assumed):
//
//   - deny by default;
//   - reads: system prefixes, the workspace, a scratch dir, and the detected
//     toolchain roots — the host home directory is NOT readable (no ~/.ssh, no
//     browser profiles, no other projects);
//   - writes: the workspace, the scratch dir, and /dev/null. Nothing else, so a
//     prompt-injected model cannot clobber the user's files;
//   - no network;
//   - no Keychain: `com.apple.securityd` is denied by name, because mach-lookup
//     would otherwise let `security find-generic-password` reach the user's
//     stored passwords even with every file read denied.
//
// sandbox-exec is marked deprecated by Apple but is still present and functional
// through macOS 26/27; when it disappears the probe fails and run_bash goes back
// to being unavailable, which is the same policy the rest of the engine already
// assumes (never fall back to an unsandboxed shell).

// execBackendEnv lets an operator force the backend off. Values other than
// "none" are ignored: there is exactly one backend on darwin, so a typo must not
// silently disable isolation.
const execBackendEnv = "DIVVY_EXEC_BACKEND"

// execReadPathsEnv adds extra readable roots (colon-separated), for toolchains
// the detector cannot guess (a venv, an SDK under a nonstandard prefix).
const execReadPathsEnv = "DIVVY_EXEC_READ_PATHS"

// darwinSystemReadPaths are always readable: the OS itself, the dynamic loader,
// and the default toolchain prefixes.
var darwinSystemReadPaths = []string{
	"/usr", "/System", "/Library", "/bin", "/sbin", "/opt",
	"/etc", "/private/etc", "/dev",
	"/private/var/db/dyld", "/private/var/select",
}

// darwinDeniedMachServices are Mach bootstrap names that would hand the sandbox
// credentials it is not supposed to have. Denying securityd also breaks the
// keychain APIs for every process in the sandbox, which is the point.
var darwinDeniedMachServices = []string{
	"com.apple.securityd",
	"com.apple.trustd",
	"com.apple.trustd.agent",
	"com.apple.SecurityServer",
}

// execDirs is the scratch space a sandboxed command may use: a private HOME/
// TMPDIR and a persistent Go build cache (the Linux backend has the same layout
// under /tmp).
type execDirs struct {
	tmp   string
	cache string
}

// darwinExecDirs creates and returns the scratch dirs. Both live under the OS
// temp dir, which the profile grants explicitly and nothing else.
func darwinExecDirs() (execDirs, error) {
	tmp := filepath.Join(os.TempDir(), "divvy-exec")
	cache := filepath.Join(os.TempDir(), "divvy-gocache")
	for _, d := range []string{tmp, cache, filepath.Join(tmp, "home"), filepath.Join(tmp, "gopath")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return execDirs{}, fmt.Errorf("sandbox scratch dir: %w", err)
		}
	}
	// Hand the resolved spelling to the sandbox: seatbelt compares the path a
	// process actually opens, and on macOS os.TempDir() says "/var/..." while
	// the kernel sees "/private/var/...". Tools inherit these values, so they
	// open the spelling the profile can match.
	return execDirs{tmp: realPath(tmp), cache: realPath(cache)}, nil
}

// isolateShellCommand wraps `/bin/sh -c command` in sandbox-exec. If the
// sandbox cannot be built the command is not started — there is no unsandboxed
// fallback.
func isolateShellCommand(ctx context.Context, root, command string, env []string) (*exec.Cmd, error) {
	if disabledByEnv() {
		return nil, fmt.Errorf("%w: run_bash is disabled by %s=none and the command was not executed", ErrExecutionUnavailable, execBackendEnv)
	}
	sbPath, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("%w: run_bash requires filesystem isolation; sandbox-exec is not available and the command was not executed", ErrExecutionUnavailable)
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
	dirs, err := darwinExecDirs()
	if err != nil {
		return nil, err
	}
	profile := darwinProfile(abs, dirs)

	cmd := exec.CommandContext(ctx, sbPath, "-p", profile, "/bin/sh", "-c", command)
	cmd.Dir = realPath(abs)
	cmd.Env = darwinEnv(env, dirs)
	return cmd, nil
}

// darwinEnv is the sandbox-side environment: allow-listed host variables plus
// scratch paths, and offline Go settings (the sandbox has no network, so a
// module download could only hang).
func darwinEnv(env []string, dirs execDirs) []string {
	out := filterIsolatedEnv(env)
	out = append(out,
		"HOME="+filepath.Join(dirs.tmp, "home"),
		"TMPDIR="+dirs.tmp,
		"TMP="+dirs.tmp,
		"TEMP="+dirs.tmp,
		"XDG_CACHE_HOME="+filepath.Join(dirs.tmp, "xdg-cache"),
		"GOCACHE="+dirs.cache,
		"GOPATH="+filepath.Join(dirs.tmp, "gopath"),
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS="+isolatedGOFlags,
	)
	// GOMODCACHE must stay on the host (it is read-only from inside), otherwise
	// every offline build with dependencies fails to find its modules.
	if dir := darwinGoModCache(); dir != "" {
		out = append(out, "GOMODCACHE="+dir)
	}
	return out
}

// darwinProfile renders the SBPL policy. Order matters: later rules override
// earlier ones, so the mach-service denials come last.
func darwinProfile(root string, dirs execDirs) string {
	read := []string{root}
	write := []string{root, dirs.tmp, dirs.cache}
	read = append(read, dirs.tmp, dirs.cache)

	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-exec*)\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	b.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n")
	// /dev/null is opened for writing by nearly every toolchain; it is a sink,
	// not a file, so allowing it does not widen write confinement.
	b.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/dtracehelper\"))\n")

	b.WriteString("(allow file-read*\n    (literal \"/\")\n")
	for _, p := range darwinSystemReadPaths {
		fmt.Fprintf(&b, "    (subpath %s)\n", sbplQuote(p))
	}
	for _, p := range dedupePaths(append(read, darwinToolchainReadPaths()...)) {
		fmt.Fprintf(&b, "    (subpath %s)\n", sbplQuote(p))
	}
	// getcwd() walks up from the working directory reading each ancestor entry;
	// without these literals every tool that calls it fails with "cannot access
	// parent directories". Literals (not subpaths) allow listing that one
	// directory, never descending into its children.
	for _, p := range dedupePaths(parentDirs(root)) {
		fmt.Fprintf(&b, "    (literal %s)\n", sbplQuote(p))
	}
	b.WriteString(")\n")

	b.WriteString("(allow file-write*\n")
	for _, p := range dedupePaths(write) {
		fmt.Fprintf(&b, "    (subpath %s)\n", sbplQuote(p))
	}
	b.WriteString(")\n")

	b.WriteString("(deny network*)\n")
	for _, svc := range darwinDeniedMachServices {
		fmt.Fprintf(&b, "(deny mach-lookup (global-name %s))\n", sbplQuote(svc))
	}
	return b.String()
}

// realPath resolves symlinks for a path that goes into the profile.
//
// Seatbelt matches on the path as the caller spelled it, not on the canonical
// one: a process that opens "/var/folders/…/divvy-exec" is checked against
// "/var/…", while a process that opens "/private/var/…" is checked against that.
// macOS makes /var and /tmp symlinks into /private, so the profile has to carry
// both spellings or half the accesses fail.
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != "" {
		return resolved
	}
	return p
}

// dedupePaths expands each path into its literal and symlink-resolved spelling,
// drops empties and duplicates, and drops any path already covered by an earlier
// entry (a subpath of it), keeping the profile small without losing a spelling.
func dedupePaths(paths []string) []string {
	var out []string
	add := func(p string) {
		if p == "" || p == "." || p == "/" {
			return
		}
		for _, kept := range out {
			if p == kept || strings.HasPrefix(p, kept+string(os.PathSeparator)) {
				return
			}
		}
		out = append(out, p)
	}
	for _, p := range paths {
		clean := filepath.Clean(strings.TrimSpace(p))
		add(clean)
		add(realPath(clean))
	}
	return out
}

// sbplQuote renders a path as an SBPL string literal.
func sbplQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// darwinToolchainOnce memoizes the expensive detection (it shells out to `go
// env`); RunBash is called per tool step, not per profile byte.
var (
	darwinToolchainOnce  sync.Once
	darwinToolchainPaths []string
	darwinGoModCachePath string
)

func darwinToolchainReadPaths() []string {
	darwinToolchainOnce.Do(func() {
		seen := map[string]bool{}
		add := func(p string) {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				return
			}
			seen[p] = true
			darwinToolchainPaths = append(darwinToolchainPaths, p)
		}
		// GOROOT/GOMODCACHE come from `go env` when go exists; both are needed
		// for an offline build and neither is a secret.
		if _, err := exec.LookPath("go"); err == nil {
			if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
				add(string(out))
			}
			if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
				dir := strings.TrimSpace(string(out))
				darwinGoModCachePath = dir
				add(dir)
			}
		}
		// A toolchain installed under the home directory (nvm, pyenv, rustup)
		// has to be readable, together with its prefix, or the build cannot
		// even start. Only the tool's own tree is exposed, never home as a
		// whole.
		for _, name := range []string{"go", "node", "npm", "yarn", "pnpm", "python3", "pip3", "cargo", "rustc", "git", "make", "cmake", "clang", "gcc", "cc"} {
			bin, err := exec.LookPath(name)
			if err != nil {
				continue
			}
			dir := filepath.Dir(bin)
			add(dir)
			if parent := filepath.Dir(dir); underHome(parent) {
				add(parent)
			}
		}
		for _, extra := range strings.Split(os.Getenv(execReadPathsEnv), ":") {
			if extra = strings.TrimSpace(extra); extra != "" {
				if abs, err := filepath.Abs(extra); err == nil {
					add(abs)
				}
			}
		}
	})
	return darwinToolchainPaths
}

func darwinGoModCache() string {
	darwinToolchainReadPaths() // ensure the memo ran
	return darwinGoModCachePath
}

func underHome(p string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "/" || p == "." {
		return false
	}
	return p == home || strings.HasPrefix(p, home+string(os.PathSeparator))
}

func disabledByEnv() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(execBackendEnv)), "none")
}

// ProbeShellIsolation reports whether run_bash can actually be isolated here.
// It does not trust the profile: it runs the sandbox and asserts that a write
// outside the workspace is refused, so a profile that silently degraded to
// "allow everything" turns execution off instead of leaving it on and lying.
func ProbeShellIsolation(root string) (bool, string) {
	if disabledByEnv() {
		return false, "run_bash is disabled by " + execBackendEnv + "=none"
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return false, "run_bash requires filesystem isolation; sandbox-exec is not available"
	}
	if strings.TrimSpace(root) == "" {
		root = os.TempDir()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, "run_bash isolation probe failed: " + err.Error()
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return false, "run_bash isolation probe failed: " + err.Error()
	}
	if err := probeDarwinSandbox(abs); err != nil {
		return false, "run_bash isolation is installed but cannot confine here: " + err.Error()
	}
	return true, ""
}

// parentDirs lists the ancestors of root, nearest first, excluding root itself
// and "/" (which the profile already allows).
func parentDirs(root string) []string {
	var out []string
	for dir := filepath.Dir(root); dir != "/" && dir != "." && dir != ""; dir = filepath.Dir(dir) {
		out = append(out, dir)
	}
	return out
}

// probeDarwinSandbox checks the properties the engine relies on, from the
// outside: the sandbox runs commands at all, it cannot write outside the
// workspace, and it cannot read a file outside the allowlist. A profile that
// silently degraded to "allow everything" fails these checks, and run_bash goes
// back to unavailable instead of running unsandboxed.
//
// Every check is judged by observable effects (file created or not, exit status)
// rather than by scraping output: the shell itself writes startup noise to
// stderr, and "output was non-empty" is not evidence of anything.
func probeDarwinSandbox(root string) error {
	outside := filepath.Join(os.TempDir(), fmt.Sprintf("divvy-probe-out-%d", os.Getpid()))
	_ = os.Remove(outside)
	// The write is expected to fail; only the stat decides.
	// The subshell keeps a redirection failure from aborting the probe shell
	// itself (a redirection error on a special builtin like `:` exits it).
	if err := runProbeShell(root, `( printf probe > `+shellQuote(outside)+` ) 2>/dev/null; true`); err != nil {
		return err
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		return fmt.Errorf("write confinement is not enforced: the sandbox created %s", outside)
	}
	if err := runProbeShell(root, `true`); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	secret := filepath.Join(home, fmt.Sprintf(".divvy-probe-read-%d", os.Getpid()))
	if err := os.WriteFile(secret, []byte("probe"), 0o600); err != nil {
		return nil // cannot set up the check; the write check above already ran
	}
	defer os.Remove(secret)
	// Exit status, not output: `cat` succeeds only if the file was readable.
	if err := runProbeShell(root, `cat `+shellQuote(secret)+` 2>/dev/null`); err == nil {
		return fmt.Errorf("read confinement is not enforced: the sandbox read %s", secret)
	}
	return nil
}

// runProbeShell runs one shell command inside the freshly built sandbox and
// returns its error (non-zero exit or start failure).
func runProbeShell(root, script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, err := isolateShellCommand(ctx, root, script, nil)
	if err != nil {
		return err
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return fmt.Errorf("%v: %s", runErr, strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

// lastLine keeps probe errors short: seatbelt prints multi-line diagnostics and
// only the final one names the denied operation.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
