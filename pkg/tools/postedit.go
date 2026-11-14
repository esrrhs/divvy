package tools

import (
	"context"
	"fmt"
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

// maxEditCheckCommands bounds how many separate checks one edit may trigger, so
// a multi-file replace cannot turn into a build sweep.
const maxEditCheckCommands = 3

// editCheckOutputCap bounds the diagnostics block appended to a tool result.
const editCheckOutputCap = 4000

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
	fmt.Fprintf(&b, "[auto-verify] %s: %s (%s)\n", strings.Join(c.Commands, " && "), verdict, c.Duration.Round(time.Millisecond))
	if c.Failed || c.TimedOut {
		b.WriteString(c.Output)
		if !strings.HasSuffix(c.Output, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("Fix these before continuing. Do not call finish while the check above fails.")
	} else {
		b.WriteString("No compile/type errors reported for the edited files.")
	}
	return b.String()
}

// VerifyEditedFiles plans and runs the cheap check for the given workspace
// paths. Paths that are not source files, or that no longer exist, are ignored.
func (s *Sandbox) VerifyEditedFiles(ctx context.Context, paths []string, timeout time.Duration) EditCheck {
	project, cmds, skip := planEditCheck(s.Root, paths)
	check := EditCheck{Project: project, Commands: cmds, Skipped: skip}
	if len(cmds) == 0 {
		return check
	}
	if reason, ok := s.ToolDisabled(ToolRunBash); ok {
		check.Commands = nil
		check.Skipped = "shell execution unavailable in this environment (" + reason + ")"
		return check
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return s.RunEditCheckCommands(ctx, project, cmds, timeout)
}

// RunEditCheckCommands executes planned checks, stopping at the first failure.
// It is exported separately so the finish gate can re-run a previously failing
// check without re-planning from file names.
func (s *Sandbox) RunEditCheckCommands(ctx context.Context, project ProjectType, cmds []string, timeout time.Duration) EditCheck {
	return runEditChecks(func(cmd string, timeout time.Duration) (*ExecResult, error) {
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

// shellQuote single-quotes a path for sh. Paths come from the model's tool
// arguments, so this must not allow command injection.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
