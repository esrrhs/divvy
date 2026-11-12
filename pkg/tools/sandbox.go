package tools

import (
	"sort"

	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultMaxReadBytes  = 64 * 1024
	defaultMaxWriteBytes = 256 * 1024
	defaultMaxList       = 120
	defaultMaxOutput     = 24 * 1024
)

// ErrExecutionUnavailable marks a failure caused by the host lacking the
// isolation primitives run_bash needs (bubblewrap/landlock) rather than by
// the command itself. Callers must treat it as fatal for the run: retrying
// or re-splitting cannot make bwrap appear, so it must not be counted as a
// verification failure or fed back to the model as something to repair.
var ErrExecutionUnavailable = errors.New("command execution unavailable in this environment")

var skipDirNames = map[string]bool{
	".git":         true,
	".divvy":       true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"coverage":     true,
}

// copySkipDirNames is the narrower set skipped when snapshotting a project
// into an isolated mirror. node_modules and vendor are copied so the
// isolated tree can run existing project tests, but they stay in
// skipDirNames for search/merge so dependency trees are not merged back
// or listed as project source.
var copySkipDirNames = map[string]bool{
	".git":   true,
	".divvy": true,
}

// IsSkippedName reports whether a directory/file name is hidden from the
// workspace snapshot, mirror listing and web file browser (.git,
// node_modules, ...). It is exported so the read-only web API applies the
// exact same ignore set as isolated mirrors.
func IsSkippedName(name string) bool { return skipDirNames[name] }

// Sandbox confines file and command operations to a workspace directory.
type Sandbox struct {
	Root      string
	Timeout   time.Duration
	MaxRead   int
	MaxWrite  int
	MaxList   int
	MaxOutput int

	// RequireActionConsent refuses tools with external side effects
	// (HTTP, downloads, browser) unless ActionConsent approves them.
	// File merge approval is not treated as this consent. If the hook
	// is nil, those tools are denied. Ask is not authorization.
	RequireActionConsent bool
	ActionConsent        func(name string, args map[string]any) (bool, string, error)

	// disabledTools maps a tool name to a stable reason it must not be
	// offered or retried in this environment (e.g. no bubblewrap).
	disabledTools map[string]string

	// Web, when set, enables outbound web_search/web_fetch. Nil keeps the
	// sandbox fully offline. Shared across mirror sandboxes (read-only).
	Web *WebClient

	// Browser, when set, enables headless Chrome tools. Nil disables them.
	Browser *BrowserClient

	// browserHeld records that this sandbox owns the BrowserClient's leaf
	// lease, so repeated browser tool calls in one leaf do not re-acquire.
	// RunWorker defers ReleaseBrowser; isolated mirrors get a fresh flag.
	browserHeld bool

	// hiddenEnv names environment variables withheld from tool subprocesses.
	// The web serve path hides LLM credentials so a leaf's `echo
	// $OPENAI_API_KEY`/`printenv` cannot read the key into tool output that
	// gets persisted to the event file. Empty makes subprocesses inherit the
	// full environment (CLI behavior; RunManager never runs in CLI mode).
	hiddenEnv []string
}

// HideEnv withholds the named environment variables from tool subprocesses.
func (s *Sandbox) HideEnv(keys ...string) {
	s.hiddenEnv = append(s.hiddenEnv, keys...)
}

// HiddenEnv returns the env keys withheld from tool subprocesses, so isolated
// mirror sandboxes inherit the same scrubbing.
func (s *Sandbox) HiddenEnv() []string { return s.hiddenEnv }

// acquireBrowserLease takes exclusive leaf-level ownership of the shared
// browser tab on first use. Browser multi-step flows (navigate → click →
// text) must not interleave across parallel leaves.
func (s *Sandbox) acquireBrowserLease(ctx context.Context) error {
	if s.Browser == nil {
		return browserDisabledErr
	}
	if s.browserHeld {
		return nil
	}
	if err := s.Browser.Acquire(ctx); err != nil {
		return err
	}
	s.browserHeld = true
	return nil
}

// ReleaseBrowser returns the leaf browser lease if this sandbox acquired it.
// Safe to defer unconditionally for every worker.
func (s *Sandbox) ReleaseBrowser() {
	if s.Browser != nil && s.browserHeld {
		s.Browser.Release()
		s.browserHeld = false
	}
}

// NewSandbox creates a workspace-rooted sandbox. root is created if missing.
func NewSandbox(root string) (*Sandbox, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, err
	}
	// Canonicalize the root: Resolve returns fully-evaluated paths, so on
	// platforms where the workspace sits under a symlink (e.g. macOS
	// /var -> /private/var) a lexical Root would make every Resolve result
	// look like it escapes the workspace and filepath.Rel would produce
	// "../../.." for paths inside it. Keeping Root real makes Rel and the
	// containment checks agree with Resolve.
	abs = mustRealRoot(abs)
	return &Sandbox{
		Root:      abs,
		Timeout:   60 * time.Second,
		MaxRead:   defaultMaxReadBytes,
		MaxWrite:  defaultMaxWriteBytes,
		MaxList:   defaultMaxList,
		MaxOutput: defaultMaxOutput,
	}, nil
}

// Rel returns a workspace-relative path for display.
func (s *Sandbox) Rel(abs string) string {
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// maxBatchReadFiles bounds how many files one read_file paths-batch may
// return, protecting the model context from an over-broad request.
const maxBatchReadFiles = 8

// ReadFiles reads several whole files in one call (each capped independently
// by MaxRead), returning them under "### path" headers. All paths are
// validated up front so a single bad name fails the whole batch instead of
// silently returning a partial list.
func (s *Sandbox) ReadFiles(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("paths must contain at least one file")
	}
	if len(paths) > maxBatchReadFiles {
		return "", fmt.Errorf("paths accepts at most %d files; narrow the list", maxBatchReadFiles)
	}
	// Validate every path up front so one bad name fails the whole batch.
	for _, p := range paths {
		a, err := s.Resolve(p)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(a)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", p)
		}
	}
	var b strings.Builder
	for i, p := range paths {
		content, err := s.ReadFile(p)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "### %s\n%s\n", filepath.ToSlash(p), content)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ReadFile reads a text file, truncating if necessary.
func (s *Sandbox) ReadFile(path string) (string, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	max := s.MaxRead
	if max <= 0 {
		max = defaultMaxReadBytes
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", err
	}
	truncated := false
	if len(data) > max {
		data = data[:max]
		truncated = true
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not valid UTF-8 text", path)
	}
	out := string(data)
	if truncated {
		out += fmt.Sprintf("\n\n[truncated to %d bytes]", max)
	}
	return out, nil
}

// maxLineReadBytes bounds the whole-file pre-read ReadFileLines performs
// before slicing out the requested window (the window output itself is
// still capped by MaxRead). Without it, asking for ten lines of a huge
// generated file would load the entire file into memory; bash (sed -n)
// remains available for windows into oversized files.
const maxLineReadBytes = 4 * 1024 * 1024

// ReadFileLines reads an inclusive 1-indexed line range of a text file.
// Each returned line is prefixed with its line number ("123: ...") so the
// model can feed exact line numbers back into replace_lines. A zero end
// reads from start to EOF. Output is capped at MaxRead bytes.
func (s *Sandbox) ReadFileLines(path string, start, end int) (string, error) {
	if start < 1 {
		return "", fmt.Errorf("start_line must be >= 1")
	}
	if end != 0 && end < start {
		return "", fmt.Errorf("end_line %d is before start_line %d", end, start)
	}
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > maxLineReadBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d-byte line-read limit; "+
			"read a window with bash (sed -n '%d,%dp') or use read_file", path, info.Size(), maxLineReadBytes, start, end)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not valid UTF-8 text", path)
	}
	lines := strings.Split(string(data), "\n")
	if start > len(lines) {
		return "", fmt.Errorf("start_line %d beyond file (%d lines)", start, len(lines))
	}
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	max := s.MaxRead
	if max <= 0 {
		max = defaultMaxReadBytes
	}
	width := len(strconv.Itoa(end))
	var b strings.Builder
	written := 0
	truncated := false
	for no := start; no <= end; no++ {
		entry := fmt.Sprintf("%*d: %s\n", width, no, lines[no-1])
		written += len(entry)
		if written > max {
			truncated = true
			break
		}
		b.WriteString(entry)
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("[truncated to %d bytes; narrow the line range]\n", max)
	}
	return out, nil
}

// WriteFile writes a whole file, creating parent directories.
func (s *Sandbox) WriteFile(path, content string) error {
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	max := s.MaxWrite
	if max <= 0 {
		max = defaultMaxWriteBytes
	}
	if len(content) > max {
		return fmt.Errorf("content exceeds %d bytes", max)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
		return err
	}
	return s.verifyStillConfined(abs)
}

// ReplaceLines replaces an inclusive 1-indexed line range with content.
func (s *Sandbox) ReplaceLines(path string, start, end int, content string) error {
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	text := string(raw)
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	lines := strings.Split(text, "\n")
	// Preserve trailing newline semantics: Split keeps a last empty element if file ends with \n.
	if start < 1 || end < start {
		return fmt.Errorf("invalid line range %d-%d", start, end)
	}
	if start > len(lines) {
		return fmt.Errorf("start_line %d beyond file (%d lines)", start, len(lines))
	}
	if end > len(lines) {
		end = len(lines)
	}
	repl := strings.ReplaceAll(content, "\r\n", "\n")
	replLines := strings.Split(repl, "\n")
	newLines := append([]string{}, lines[:start-1]...)
	newLines = append(newLines, replLines...)
	newLines = append(newLines, lines[end:]...)
	out := strings.Join(newLines, nl)
	if strings.HasSuffix(string(raw), "\n") && !strings.HasSuffix(out, nl) {
		out += nl
	}
	return os.WriteFile(abs, []byte(out), 0644)
}

// ReplaceText replaces old with new. If replaceAll is false, old must occur exactly once.
func (s *Sandbox) ReplaceText(path, old, new string, replaceAll bool) error {
	if old == "" {
		return fmt.Errorf("old_string must not be empty")
	}
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	text := string(raw)
	n := strings.Count(text, old)
	if n == 0 {
		return fmt.Errorf("old_string not found in %s", path)
	}
	if !replaceAll && n != 1 {
		return fmt.Errorf("old_string occurs %d times in %s; make it unique or set replace_all", n, path)
	}
	var out string
	if replaceAll {
		out = strings.ReplaceAll(text, old, new)
	} else {
		out = strings.Replace(text, old, new, 1)
	}
	return os.WriteFile(abs, []byte(out), 0644)
}

// TextEdit is one exact-string replacement in a ReplaceTexts batch.
type TextEdit struct {
	Old string
	New string
}

// ReplaceTexts applies several exact-string replacements to one file
// atomically: every edit is validated against, and applied to, an in-memory
// copy first, and the file is rewritten only after all edits succeed. A
// missing or ambiguous anchor in any edit therefore leaves the file
// completely untouched, so a multi-edit call can never half-apply.
func (s *Sandbox) ReplaceTexts(path string, edits []TextEdit, replaceAll bool) error {
	if len(edits) == 0 {
		return fmt.Errorf("edits must contain at least one edit")
	}
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	text := string(raw)
	for i, ed := range edits {
		if ed.Old == "" {
			return fmt.Errorf("edits[%d]: old_string must not be empty", i)
		}
		n := strings.Count(text, ed.Old)
		if n == 0 {
			return fmt.Errorf("edits[%d]: old_string not found in %s", i, path)
		}
		if !replaceAll && n != 1 {
			return fmt.Errorf("edits[%d]: old_string occurs %d times in %s; make it unique or set replace_all", i, n, path)
		}
		if replaceAll {
			text = strings.ReplaceAll(text, ed.Old, ed.New)
		} else {
			text = strings.Replace(text, ed.Old, ed.New, 1)
		}
	}
	return os.WriteFile(abs, []byte(text), 0644)
}

// DeletePath removes a workspace file or directory. Directories require
// recursive=true; the workspace root itself can never be deleted.
func (s *Sandbox) DeletePath(path string, recursive bool) error {
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	if abs == s.Root {
		return fmt.Errorf("refusing to delete the workspace root")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() && !recursive {
		return fmt.Errorf("%s is a directory; set recursive true to delete it", path)
	}
	if recursive {
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

// MovePath renames a file or directory inside the workspace. Replacing an
// existing destination file is allowed; an existing destination directory is
// rejected, as is moving a directory into its own subtree.
func (s *Sandbox) MovePath(from, to string) error {
	src, err := s.Resolve(from)
	if err != nil {
		return err
	}
	dst, err := s.Resolve(to)
	if err != nil {
		return err
	}
	if src == s.Root {
		return fmt.Errorf("refusing to move the workspace root")
	}
	if src == dst {
		return fmt.Errorf("source and destination are the same: %s", from)
	}
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		return fmt.Errorf("destination already exists as a directory: %s", to)
	}
	// Moving a directory into itself or one of its descendants would corrupt
	// the tree; filepath.Rel(src, dst) is a plain name only when dst is
	// inside src (siblings produce a "../" prefix).
	if rel, err := filepath.Rel(src, dst); err == nil && rel != "." &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("cannot move %s into its own subtree: %s", from, to)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// ListDir lists files under path.
func (s *Sandbox) ListDir(path string, recursive bool) (string, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return s.Rel(abs), nil
	}
	max := s.MaxList
	if max <= 0 {
		max = defaultMaxList
	}

	var b strings.Builder
	count := 0
	truncated := false

	if !recursive {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if skipDirNames[e.Name()] {
				continue
			}
			count++
			if count > max {
				truncated = true
				break
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			fmt.Fprintf(&b, "%s\n", name)
		}
	} else {
		err = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == abs {
				return nil
			}
			name := d.Name()
			if d.IsDir() && skipDirNames[name] {
				return filepath.SkipDir
			}
			if skipDirNames[name] {
				return nil
			}
			count++
			if count > max {
				truncated = true
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(abs, p)
			if d.IsDir() {
				fmt.Fprintf(&b, "%s/\n", rel)
			} else {
				fmt.Fprintf(&b, "%s\n", rel)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("[truncated to %d entries]\n", max)
	}
	return out, nil
}

// Snapshot is a short workspace listing for prompt injection.
func (s *Sandbox) Snapshot() string {
	out, err := s.ListDir(".", true)
	if err != nil {
		return "(unable to list workspace: " + err.Error() + ")"
	}
	return out
}

// ExecResult is the outcome of a shell command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// RunBash executes command in the workspace with a timeout.
func (s *Sandbox) RunBash(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("empty command")
	}
	if timeout <= 0 {
		timeout = s.Timeout
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := os.Environ()
	if len(s.hiddenEnv) > 0 {
		drop := make(map[string]bool, len(s.hiddenEnv))
		for _, k := range s.hiddenEnv {
			drop[k] = true
		}
		filtered := make([]string, 0, len(env))
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i >= 0 && drop[kv[:i]] {
				continue
			}
			filtered = append(filtered, kv)
		}
		env = filtered
	}
	cmd, err := isolateShellCommand(ctx, s.Root, command, env)
	if err != nil {
		return nil, err
	}
	// Run the shell in its own process group (POSIX) / hidden console
	// (Windows) so a timeout can kill the whole shell tree, including
	// background children (e.g. a started server). Killing only sh leaves
	// such children alive and sh waits on them forever, which makes the
	// timeout ineffective and the leaf hang.
	configureLeafCommand(cmd)
	cmd.Cancel = func() error {
		// If Start never launched (e.g. fork failed under load),
		// cmd.Process is nil and there is nothing to signal.
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killLeafProcessTree(cmd.Process.Pid)
	}
	// Give killed processes a moment to be reaped.
	cmd.WaitDelay = 2 * time.Second

	// After the shell exits — even on success — kill any process still in its
	// group. A background child started in a subshell is reparented to pid 1
	// while the outer shell returns successfully, so neither a timeout nor
	// sh's exit would otherwise stop it and the server leaks forever. A normal
	// foreground command leaves the group empty; ESRCH is ignored. Guard the
	// nil-Process case (failed Start) so a launch error surfaces instead of
	// being masked by a panic in the deferred cleanup.
	defer func() {
		if cmd.Process == nil {
			return
		}
		_ = killLeafProcessTree(cmd.Process.Pid)
	}()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = capWriter{w: &stdout, n: s.maxOut()}
	cmd.Stderr = capWriter{w: &stderr, n: s.maxOut()}

	err = cmd.Run()
	res := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		// Make the hang reason visible so a leaf can correct its command.
		if res.Stderr == "" {
			res.Stderr = fmt.Sprintf("command timed out after %s (process group killed)", timeout)
		}
		return res, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func (s *Sandbox) maxOut() int {
	if s.MaxOutput <= 0 {
		return defaultMaxOutput
	}
	return s.MaxOutput
}

type capWriter struct {
	w *bytes.Buffer
	n int
}

func (c capWriter) Write(p []byte) (int, error) {
	remain := c.n - c.w.Len()
	if remain <= 0 {
		return len(p), nil
	}
	if len(p) > remain {
		c.w.Write(p[:remain])
		c.w.WriteString("\n[output truncated]")
		return len(p), nil
	}
	return c.w.Write(p)
}

// DisableTool removes a tool from this sandbox's advertised set and makes
// Call refuse it with a stable reason. Use this after a real capability
// probe, not as a substitute for isolation.
func (s *Sandbox) DisableTool(name, reason string) {
	if s == nil {
		return
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	if s.disabledTools == nil {
		s.disabledTools = map[string]string{}
	}
	if strings.TrimSpace(reason) == "" {
		reason = name + " is unavailable in this environment"
	}
	s.disabledTools[name] = reason
}

// ToolDisabled reports whether name was disabled and why.
func (s *Sandbox) ToolDisabled(name string) (string, bool) {
	if s == nil || s.disabledTools == nil {
		return "", false
	}
	reason, ok := s.disabledTools[strings.ToLower(strings.TrimSpace(name))]
	return reason, ok
}

// UnavailableNotice is a message for the model/UI listing tools that will
// not work in this environment, so the loop should not spend its budget
// retrying them.
func (s *Sandbox) UnavailableNotice() string {
	if s == nil || len(s.disabledTools) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.disabledTools))
	for n := range s.disabledTools {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("The following tools are unavailable in this environment and must not be called or retried:")
	for _, n := range names {
		fmt.Fprintf(&b, "\n- %s: %s", n, s.disabledTools[n])
	}
	return b.String()
}

func (s *Sandbox) FilterNativeTools(raw []map[string]any) []map[string]any {
	if s == nil || len(s.disabledTools) == 0 {
		return raw
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if _, disabled := s.ToolDisabled(name); disabled {
			continue
		}
		out = append(out, m)
	}
	return out
}

// CopyLimits copies capability and consent policy onto dst (used when
// adopting a manual-approval mirror sandbox).
func (s *Sandbox) CopyLimits(dst *Sandbox) {
	if s == nil || dst == nil {
		return
	}
	for name, reason := range s.disabledTools {
		dst.DisableTool(name, reason)
	}
	dst.RequireActionConsent = s.RequireActionConsent
	if s.ActionConsent != nil {
		dst.ActionConsent = s.ActionConsent
	}
}
