package tools

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Post-edit verification
//
// A model that edits code and never builds it has not observed anything: it is
// guessing. The tool loop therefore runs the cheapest meaningful check for the
// files a tool just touched and hands the result back as part of that tool's
// output. This is the "observe" half of the loop, done deterministically
// instead of hoping the model remembers to run `go build`.
//
// It is deliberately not a full test run: the point is fast, local, per-edit
// feedback (syntax/type/compile), not acceptance. Acceptance stays with the
// verifier (tree mode DoD) and with explicit run_bash calls.

// editCheckDuration renders a check's duration. In-process parses finish in
// microseconds, and "0s" reads like the check did not run.
func editCheckDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	return d.Round(time.Millisecond).String()
}

// maxEditCheckCommands bounds how many separate checks one edit may trigger, so
// a multi-file replace cannot turn into a build sweep.
const maxEditCheckCommands = 3

// editCheckOutputCap bounds the diagnostics block appended to a tool result.
const editCheckOutputCap = 4000

// builtinGoSyntaxPrefix marks a check that runs in-process instead of through
// the shell.
//
// Why a string prefix instead of a type: the finish gate remembers "what to
// re-run" as a []string and replays it through the same entry point, so the
// command list has to survive a round trip. "go syntax <file>" is not a real go
// subcommand, so if interception ever regressed the worst case is a loud error,
// never a stray command doing something.
const builtinGoSyntaxPrefix = "go syntax "

// maxSyntaxCheckBytes skips files too large to parse cheaply; the editor would
// have truncated them anyway.
const maxSyntaxCheckBytes = 2 << 20

// EditCheck is the outcome of a post-edit verification.
type EditCheck struct {
	Project  ProjectType
	Commands []string
	Failed   bool
	TimedOut bool
	// Skipped explains why nothing ran (unsupported project, no runnable
	// check, or shell execution unavailable). Empty means a check ran.
	Skipped  string
	Output   string
	Duration time.Duration
}

// Ran reports whether at least one command was executed.
func (c EditCheck) Ran() bool { return len(c.Commands) > 0 }

// SyntaxOnly reports whether every check that ran was an in-process syntax
// parse rather than a real build. The block wording differs: claiming "no
// compile errors" from a parse-only check would be a lie.
func (c EditCheck) SyntaxOnly() bool {
	if len(c.Commands) == 0 {
		return false
	}
	for _, cmd := range c.Commands {
		if !strings.HasPrefix(cmd, builtinGoSyntaxPrefix) {
			return false
		}
	}
	return true
}

// Block renders the check as the text appended to a tool result. It is written
// for the model: short, first line states verdict + command.
func (c EditCheck) Block() string {
	if !c.Ran() {
		if c.Skipped == "" {
			return ""
		}
		return "[auto-verify] skipped: " + c.Skipped
	}
	verdict := "OK"
	if c.TimedOut {
		verdict = "TIMED OUT"
	} else if c.Failed {
		verdict = "FAILED"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[auto-verify] %s: %s (%s)\n", strings.Join(c.Commands, " && "), verdict, editCheckDuration(c.Duration))
	if c.Failed || c.TimedOut {
		b.WriteString(c.Output)
		if !strings.HasSuffix(c.Output, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("Fix these before continuing. Do not call finish while the check above fails.")
		return b.String()
	}
	if c.SyntaxOnly() {
		// Be explicit about how shallow this is: a parse pass says nothing about
		// types, imports, or whether the tests pass.
		b.WriteString("Syntax parses; this environment could not run a real build, so type and test errors are still unknown.")
	} else {
		b.WriteString("No compile/type errors reported for the edited files.")
	}
	return b.String()
}

// VerifyEditedFiles plans and runs the cheap check for the given workspace
// paths. Paths that are not source files, or that no longer exist, are ignored.
func (s *Sandbox) VerifyEditedFiles(ctx context.Context, paths []string, timeout time.Duration) EditCheck {
	project, cmds, skip := planEditCheck(s.Root, paths)
	if reason, noShell := s.ToolDisabled(ToolRunBash); noShell {
		// The planned build commands cannot run. Fall back to the in-process
		// syntax pass, which needs no isolation backend and is the only
		// observation available on this host. Calling that a "build" would be a
		// lie, so it is labelled as a parse (see EditCheck.SyntaxOnly) — and it
		// applies even when the project type is not Go, because a Go file in a
		// C++ repo still has to parse.
		if syntax := goSyntaxCommands(s.Root, paths); len(syntax) > 0 {
			cmds, skip = syntax, ""
		} else {
			if skip == "" {
				skip = "no in-process check applies to these files"
			}
			return EditCheck{Project: project, Skipped: skip + "; shell execution unavailable (" + reason + ")"}
		}
	}
	if len(cmds) == 0 {
		return EditCheck{Project: project, Skipped: skip}
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return s.RunEditCheckCommands(ctx, project, cmds, timeout)
}

// RunEditCheckCommands executes planned checks, stopping at the first failure.
// It is exported separately so the finish gate can re-run a previously failing
// check without re-planning from file names.
//
// Builtin entries (see builtinGoSyntaxPrefix) run in-process; everything else
// goes through the shell.
func (s *Sandbox) RunEditCheckCommands(ctx context.Context, project ProjectType, cmds []string, timeout time.Duration) EditCheck {
	return runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
		if file, ok := goSyntaxTarget(cmd); ok {
			return s.checkGoSyntax(file), nil
		}
		return s.RunBash(ctx, cmd, timeout)
	}, project, cmds, timeout)
}

// runEditChecks is the testable core: a runner is any function that executes one
// command, which lets tests exercise failure/timing paths without a real shell.
func runEditChecks(run func(cmd string, timeout time.Duration) (*ExecResult, error), project ProjectType, cmds []string, timeout time.Duration) EditCheck {
	check := EditCheck{Project: project, Commands: cmds}
	var raw strings.Builder
	start := time.Now()
	for _, cmd := range cmds {
		res, err := run(cmd, timeout)
		if err != nil {
			check.Skipped = "could not run " + cmd + ": " + err.Error()
			check.Commands = nil
			return check
		}
		if res == nil {
			continue
		}
		if res.TimedOut {
			check.Failed = true
			check.TimedOut = true
			fmt.Fprintf(&raw, "$ %s\n(timed out after %s)\n%s%s\n", cmd, timeout, res.Stdout, res.Stderr)
			break
		}
		if res.ExitCode != 0 {
			check.Failed = true
			fmt.Fprintf(&raw, "$ %s\n%s%s\n", cmd, res.Stdout, res.Stderr)
			break
		}
	}
	check.Duration = time.Since(start)
	if check.Failed {
		check.Output = renderEditCheckOutput(project, raw.String())
	}
	return check
}

// renderEditCheckOutput puts the compacted root causes first (the raw output is
// clipped from the head by consumers) and caps the block.
func renderEditCheckOutput(project ProjectType, raw string) string {
	out := raw
	if compact := CompactDiagnostics(project, raw); compact != "" {
		out = compact + "\n" + raw
	} else if project != ProjectGo && project != ProjectPython && project != ProjectNode && project != ProjectRust {
		// No diagnosis rules for this toolchain: keep the tail, where the
		// actual error usually is.
		if len(out) > editCheckOutputCap {
			out = "..." + out[len(out)-editCheckOutputCap:]
		}
	}
	if len(out) > editCheckOutputCap {
		out = out[:editCheckOutputCap] + "\n...[auto-verify output truncated]"
	}
	return strings.TrimRight(out, "\n")
}

// planEditCheck picks the cheapest command that would notice a broken edit.
// Empty commands plus a reason means "nothing cheap applies here", which is
// reported to the model instead of silently pretending the edit was verified.
func planEditCheck(root string, paths []string) (ProjectType, []string, string) {
	project := DetectProject(root)
	var cmds []string
	seen := map[string]bool{}
	add := func(cmd string) {
		if cmd == "" || seen[cmd] || len(cmds) >= maxEditCheckCommands {
			return
		}
		seen[cmd] = true
		cmds = append(cmds, cmd)
	}

	var goDirs []string
	var goTestDirs []string
	var pyFiles, nodeFiles []string
	sawRust := false

	for _, p := range paths {
		rel := filepath.ToSlash(strings.TrimSpace(p))
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || strings.HasPrefix(rel, "../") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			continue // deleted or outside the workspace
		}
		switch ext := strings.ToLower(path.Ext(rel)); ext {
		case ".go":
			if !underAnyDir(rel, "vendor", "testdata", "node_modules") {
				dir := path.Dir(rel)
				if dir == "." {
					dir = ""
				}
				if strings.HasSuffix(rel, "_test.go") {
					goTestDirs = append(goTestDirs, dir)
				} else {
					goDirs = append(goDirs, dir)
				}
			}
		case ".py":
			if project == ProjectPython {
				pyFiles = append(pyFiles, rel)
			}
		case ".js", ".mjs", ".cjs":
			if project == ProjectNode {
				nodeFiles = append(nodeFiles, rel)
			}
		case ".rs":
			sawRust = true
		}
	}

	switch project {
	case ProjectGo:
		for _, dir := range goDirs {
			add(goBuildCommand(dir))
		}
		for _, dir := range goTestDirs {
			add(goVetCommand(dir))
		}
		if len(cmds) == 0 {
			return project, nil, "no package build applies to the edited files"
		}
		return project, cmds, ""
	case ProjectPython:
		if len(pyFiles) == 0 {
			return project, nil, "edited files are not Python source"
		}
		for _, f := range pyFiles {
			add(pythonSyntaxCommand(f))
		}
	case ProjectNode:
		if len(nodeFiles) == 0 {
			return project, nil, "no single-file check for the edited JavaScript/TypeScript files"
		}
		for _, f := range nodeFiles {
			add("node --check " + shellQuote(f))
		}
	case ProjectRust:
		if !sawRust {
			return project, nil, "edited files are not Rust source"
		}
		add("cargo check --quiet")
	default:
		return project, nil, "no cheap compile check for a " + string(project) + " workspace"
	}
	if len(cmds) == 0 {
		return project, nil, "no applicable check"
	}
	return project, cmds, ""
}

func goBuildCommand(dir string) string {
	if dir == "" {
		return "go build ."
	}
	return "go build ./" + shellQuote(dir)
}

func goVetCommand(dir string) string {
	if dir == "" {
		return "go vet ."
	}
	return "go vet ./" + shellQuote(dir)
}

// pythonSyntaxCommand parses the file instead of running py_compile: py_compile
// writes __pycache__ into the user's workspace, which would show up as an
// unrelated change in the turn diff.
func pythonSyntaxCommand(file string) string {
	return "python3 -c 'import ast,sys; ast.parse(open(sys.argv[1]).read())' " + shellQuote(file)
}

func underAnyDir(rel string, dirs ...string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// goSyntaxCommands lists the in-process parse checks for the edited Go files.
// Only Go is covered: go/parser ships with the toolchain we already link, while
// Python/Node/Rust would need an interpreter or compiler (i.e. the shell we do
// not have). Returning nothing is honest — the caller reports a skip.
func goSyntaxCommands(root string, paths []string) []string {
	var cmds []string
	seen := map[string]bool{}
	for _, p := range paths {
		rel := filepath.ToSlash(strings.TrimSpace(p))
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || strings.HasPrefix(rel, "../") || !strings.HasSuffix(strings.ToLower(rel), ".go") {
			continue
		}
		if underAnyDir(rel, "vendor", "testdata", "node_modules") || seen[rel] {
			continue
		}
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxSyntaxCheckBytes {
			continue
		}
		seen[rel] = true
		cmds = append(cmds, builtinGoSyntaxPrefix+rel)
		if len(cmds) >= maxEditCheckCommands {
			break
		}
	}
	return cmds
}

// goSyntaxTarget unwraps a builtin parse entry into the file it checks.
func goSyntaxTarget(cmd string) (string, bool) {
	if !strings.HasPrefix(cmd, builtinGoSyntaxPrefix) {
		return "", false
	}
	file := strings.TrimSpace(strings.TrimPrefix(cmd, builtinGoSyntaxPrefix))
	if file == "" {
		return "", false
	}
	return file, true
}

// checkGoSyntax parses one file and reports failures in the same file:line:col
// shape a compiler would, so the existing diagnosis compaction picks them up.
// It never touches the shell, so it is the one check available on a host
// without an isolation backend.
func (s *Sandbox) checkGoSyntax(rel string) *ExecResult {
	abs, err := s.Resolve(rel)
	if err != nil {
		return &ExecResult{ExitCode: 1, Stderr: err.Error()}
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return &ExecResult{ExitCode: 1, Stderr: err.Error()}
	}
	if len(src) > maxSyntaxCheckBytes {
		return &ExecResult{}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.AllErrors); err != nil {
		return &ExecResult{ExitCode: 1, Stderr: err.Error() + "\n"}
	}
	return &ExecResult{}
}

// shellQuote single-quotes a path for sh. Paths come from the model's tool
// arguments, so this must not allow command injection.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
