package agent

import (
	"path"
	"strings"

	"github.com/esrrhs/divvy/pkg/engine"
)

// acceptAnalysis is what a goal-level acceptance command demands from the
// task tree.
//
// The command is read as a CHAIN of simple commands, because that is what it
// is: `go build -o app . && ./app test.json` builds a binary and then runs it
// against a fixture. Reading the whole thing as one long token soup is how
// the earlier version of this check came to believe that "&&" was a file the
// tree had to produce — a blocker nobody can fix, which burned the replan
// budget on every run that used a chain.
type acceptAnalysis struct {
	// required: paths that must exist as tree deliverables (leaf outputs).
	// A path the command itself creates is not required, and neither is a
	// path outside the workspace: no leaf can own /tmp/app.
	required []string
	// inputs: data files the command reads. They have to come from a leaf
	// output, from the workspace as it was seeded, or from an earlier link of
	// the same chain — otherwise the acceptance cannot pass no matter how good
	// the work was.
	inputs []string
	// produced: paths the chain creates on its own.
	produced []string
}

// analyzeAcceptance models the goal-level acceptance commands as one chain.
func analyzeAcceptance(cmds []string) acceptAnalysis {
	a := acceptAnalysis{}
	produced := map[string]bool{}
	seenReq := map[string]bool{}
	seenIn := map[string]bool{}

	type segment struct{ fields []string }
	var segments []segment
	for _, cmd := range cmds {
		for _, seg := range splitCommandChain(cmd) {
			fields := commandFields(seg)
			if len(fields) == 0 {
				continue
			}
			segments = append(segments, segment{fields: fields})
			for _, p := range commandProducedPaths(fields) {
				if n := engine.NormalizeOutputPath(p); n != "" {
					produced[n] = true
					a.produced = append(a.produced, p)
				}
			}
		}
	}

	for _, seg := range segments {
		for _, p := range segmentRequiredPaths(seg.fields) {
			n := engine.NormalizeOutputPath(p)
			if n == "" || produced[n] || seenReq[n] {
				continue
			}
			// A path outside the workspace is not something a leaf can own.
			// Demanding it would be a blocker nobody can ever clear.
			if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
				continue
			}
			seenReq[n] = true
			a.required = append(a.required, p)
		}
		for _, p := range segmentInputPaths(seg.fields) {
			n := engine.NormalizeOutputPath(p)
			if n == "" || produced[n] || seenIn[n] || seenReq[n] {
				continue
			}
			seenIn[n] = true
			a.inputs = append(a.inputs, p)
		}
	}
	return a
}

// goalCommandPaths returns the deliverable paths named by the goal-level
// acceptance that the tree has to produce.
//
// This is the check that would have caught "go run ./cmd/demo" with no
// cmd/demo task: the acceptance command and the task tree are produced by two
// independent model calls and nothing else compares them.
func goalCommandPaths(cmds []string) []string {
	return analyzeAcceptance(cmds).required
}

// splitCommandChain breaks a shell command into its simple commands. Only the
// operators that actually join commands are honoured; everything else (quotes
// aside) is left to the shell.
func splitCommandChain(cmd string) []string {
	repl := strings.NewReplacer("&&", "\n", "||", "\n", "|", "\n", ";", "\n")
	var out []string
	for _, part := range strings.Split(repl.Replace(cmd), "\n") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// commandFields splits a simple command into words and drops the leading
// environment assignments ("GOFLAGS=-mod=mod go run ./x").
func commandFields(seg string) []string {
	fields := strings.Fields(seg)
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}
	return fields
}

// commandProducedPaths returns the paths a single simple command creates by
// itself: build outputs, redirection targets, copy destinations.
func commandProducedPaths(fields []string) []string {
	var out []string
	// `go build ./cmd/server` (no -o) drops a binary named after the package
	// in the current directory. Without this, `go build ./cmd/x && ./x` would
	// look like a command that runs a file nobody produces.
	if len(fields) >= 2 && fields[0] == "go" && fields[1] == "build" && !hasOutputFlag(fields) {
		if pkg := lastNonFlagArg(fields[2:]); pkg != "" {
			base := path.Base(pkg)
			if base != "." && base != ".." && base != "..." {
				out = append(out, "./"+base)
			}
		}
	}

	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-o" || f == "--output":
			if i+1 < len(fields) {
				out = append(out, fields[i+1])
				i++
			}
		case strings.HasPrefix(f, "-o="):
			out = append(out, strings.TrimPrefix(f, "-o="))
		case strings.HasPrefix(f, "--output="):
			out = append(out, strings.TrimPrefix(f, "--output="))
		case f == ">" || f == ">>":
			if i+1 < len(fields) {
				out = append(out, fields[i+1])
				i++
			}
		case strings.HasPrefix(f, ">"):
			out = append(out, strings.TrimPrefix(f, ">"))
		case f == "tee":
			if v, ok := firstNonFlagArg(fields[i+1:]); ok {
				out = append(out, v)
			}
		}
	}

	switch fields[0] {
	case "cp", "mv", "install", "rsync":
		// The destination of a copy is the last word.
		if dst := lastNonFlagArg(fields[1:]); dst != "" {
			out = append(out, dst)
		}
	}
	return out
}

func hasOutputFlag(fields []string) bool {
	for _, f := range fields {
		if f == "-o" || f == "--output" || strings.HasPrefix(f, "-o=") ||
			strings.HasPrefix(f, "--output=") {
			return true
		}
	}
	return false
}

func lastNonFlagArg(args []string) string {
	out := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = a
	}
	return out
}

func firstNonFlagArg(args []string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, true
	}
	return "", false
}

// segmentRequiredPaths returns the deliverable paths a simple command needs to
// exist. Only unambiguous forms are recognized; anything that names a package
// pattern (go test ./..., go build ./...) or the module root yields nothing,
// because a package-wide check cannot point at one missing file and guessing
// would produce false blockers.
func segmentRequiredPaths(fields []string) []string {
	var out []string
	switch {
	case len(fields) >= 3 && fields[0] == "go" && fields[1] == "run":
		if isDeliverablePath(fields[2]) {
			out = append(out, fields[2])
		}
	case len(fields) >= 3 && fields[0] == "go" && fields[1] == "build":
		for i := 2; i < len(fields); i++ {
			f := fields[i]
			// -o names the build OUTPUT, not a package. Treating its value
			// as a required path would flag every "go build -o bin/app ./x".
			if f == "-o" || f == "--output" {
				i++
				continue
			}
			if strings.HasPrefix(f, "-") {
				continue
			}
			if isDeliverablePath(f) {
				out = append(out, f)
				break
			}
		}
	case isExecPath(fields[0]):
		if isDeliverablePath(fields[0]) {
			out = append(out, fields[0])
		}
	case len(fields) >= 2:
		switch fields[0] {
		case "python", "python3", "node", "deno", "bun", "sh", "bash", "zsh", "pwsh":
			if isDeliverablePath(fields[1]) {
				out = append(out, fields[1])
			}
		}
	}
	return out
}

// segmentInputPaths returns the data files a simple command reads.
//
// Only data/config extensions count. Source files are the OUTPUT side's
// business (a leaf produces them), and tool-generated reports (junit xml,
// coverage json) would be flagged as missing inputs — a safety check that
// cries wolf is a safety check people turn off.
func segmentInputPaths(fields []string) []string {
	var out []string
	for _, raw := range fields {
		tok := strings.Trim(raw, `"'`)
		if !isInputCandidate(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// inputDataExts limits the input check to files that mean "data a command
// consumed".
var inputDataExts = map[string]bool{
	"json": true, "yaml": true, "yml": true, "toml": true, "ini": true,
	"cfg": true, "conf": true, "env": true, "csv": true, "tsv": true,
	"txt": true,
}

// buildOutputDirs are directories whose contents are produced by a toolchain,
// not by a task. Reasoning about what gradle/cmake/cargo drop in there is
// guesswork, and a wrong guess blocks a plan that is fine.
var buildOutputDirs = map[string]bool{
	"build": true, "dist": true, "target": true, "bin": true, "out": true,
	"node_modules": true, "vendor": true, "coverage": true, ".cache": true,
	".git": true,
}

// isInputCandidate reports whether a token names a data file the command
// consumes. Everything excluded here is excluded on purpose: an absolute path
// would drag in /dev/null and /usr/share; a token with shell metacharacters is
// the shell's business, not ours.
func isInputCandidate(tok string) bool {
	if tok == "" || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "/") ||
		strings.HasPrefix(tok, "~") {
		return false
	}
	if strings.ContainsAny(tok, "|&;<>$*?()[]{}`\\") || strings.Contains(tok, "://") ||
		strings.Contains(tok, "=") {
		return false
	}
	base := tok
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." || base == ".." {
		return false
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return false
	}
	if !inputDataExts[strings.ToLower(base[dot+1:])] {
		return false
	}
	seg := strings.SplitN(strings.TrimPrefix(tok, "./"), "/", 2)[0]
	return !buildOutputDirs[seg]
}
