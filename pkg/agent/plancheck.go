package agent

import (
	"fmt"
	"strings"

	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// planWarnings reviews the planned tree for weak contracts and DoD so that
// problems surface before any leaf burns tokens. Returns human-readable
// warnings keyed by leaf.
func (o *Orchestrator) planWarnings() []string {
	var warns []string
	claimed := map[string]string{} // "parentID|output" -> leafID

	for _, leaf := range o.tree.Leaves() {
		id := leaf.ID
		// Repair leaves are not planned work: they are injected in response to a
		// broken aggregate, they edit files that already exist (so they declare
		// no outputs by design), and their DoD is the aggregate command itself.
		// Warning about their contract would be warning about the fix.
		if strings.HasPrefix(id, waveRepairPrefix) {
			continue
		}
		switch {
		case len(leaf.DoD.Commands) == 0:
			warns = append(warns, fmt.Sprintf("%s (%s): no verification commands", id, leaf.Title))
		case placeholderDoD(leaf.DoD.Commands):
			warns = append(warns, fmt.Sprintf("%s (%s): verification is only a placeholder (%s)",
				id, leaf.Title, strings.Join(leaf.DoD.Commands, ", ")))
		}
		if len(leaf.Contract.Outputs) == 0 {
			warns = append(warns, fmt.Sprintf("%s (%s): no declared outputs", id, leaf.Title))
		}
		for _, out := range leaf.Contract.Outputs {
			key := leaf.ParentID + "|" + out
			if prev, ok := claimed[key]; ok && prev != id {
				warns = append(warns, fmt.Sprintf("%s and %s (siblings) both declare output %s; isolated runs would conflict", prev, id, out))
			} else {
				claimed[key] = id
			}
		}
	}

	// Runnable deliverable checks: a goal for a program/server/CLI must have
	// a leaf that produces an entry point and a goal-level acceptance DoD.
	if goalIsRunnable(o.tree.Goal) {
		if !o.treeHasEntryPoint() {
			warns = append(warns, "runnable goal but no leaf declares an entry point (e.g. main.go); deliverable cannot start")
		}
		if root, ok := o.tree.CloneNode(o.tree.RootID); ok && len(root.DoD.Commands) == 0 {
			warns = append(warns, "no goal-level acceptance commands for a runnable deliverable")
		}
	}
	return warns
}

// goalIsRunnable reports whether the goal asks for a program that must run
// (server/API/CLI/daemon), as opposed to a library or a one-off change.
func goalIsRunnable(goal string) bool {
	g := strings.ToLower(goal)
	keywords := []string{
		"http", "api", "server", "服务", "服务端", "监听", "程序", "cli",
		"daemon", "命令行", "可执行", "web ", "微服务", "listen",
	}
	for _, k := range keywords {
		if strings.Contains(g, k) {
			return true
		}
	}
	return false
}

// treeHasEntryPoint reports whether any leaf produces an executable entry
// file for the detected toolchain (Go main.go, Node index/server/app/main.js,
// Python main/app/server.py, Rust main.rs).
func (o *Orchestrator) treeHasEntryPoint() bool {
	entryNames := map[tools.ProjectType]map[string]bool{
		tools.ProjectGo:     setOf("main.go"),
		tools.ProjectNode:   setOf("index.js", "server.js", "app.js", "main.js"),
		tools.ProjectPython: setOf("main.py", "app.py", "server.py"),
		tools.ProjectRust:   setOf("main.rs"),
	}
	names := entryNames[o.projectType()]
	if names == nil {
		// Unknown/generic: fall back to the Go-style check.
		names = entryNames[tools.ProjectGo]
	}
	for _, leaf := range o.tree.Leaves() {
		for _, out := range leaf.Contract.Outputs {
			p := strings.ToLower(strings.TrimSpace(out))
			base := p
			if i := strings.LastIndex(p, "/"); i >= 0 {
				base = p[i+1:]
			}
			if names[base] {
				return true
			}
		}
	}
	return false
}

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// placeholderDoD reports whether every command is the do-nothing default.
func placeholderDoD(cmds []string) bool {
	for _, c := range cmds {
		if strings.TrimSpace(c) != "ls" {
			return false
		}
	}
	return true
}

// planBlockers returns structural defects that no amount of leaf-level work
// can repair. They differ from planWarnings in kind, not degree: a warning is
// advice a human may ignore, a blocker means the run is already doomed.
//
// The check is deliberately narrow. It only fires when a goal-level acceptance
// command names a concrete deliverable path ("go run ./cmd/demo", "./bin/app",
// "python app.py") that no leaf claims to produce. That is exactly the failure
// mode the tree cannot absorb: `handleAcceptanceFailure` finds no integration
// leaf to route the error to, so the root goes straight to FAILED and -resume
// can never make progress — the process just exits non-zero forever.
//
// Note what is NOT used as a gate: guessing from the goal's wording whether it
// "sounds runnable". If the root DoD runs a concrete path, that path must
// exist, no matter how the goal is phrased. A missing entry point on its own
// still stays a warning — plenty of goals ("fix this bug") need no new entry
// file, and blocking those would be worse than the disease.
func (o *Orchestrator) planBlockers() []string {
	root, ok := o.tree.CloneNode(o.tree.RootID)
	if !ok {
		return nil
	}
	// A goal with no acceptance commands is verified by ensureGoalDoD's
	// build-only fallback, which cannot reference a missing path.
	if len(root.DoD.Commands) == 0 {
		return nil
	}
	o.log.Debugf("plan structure: goal=%q runnable=%v state=%s children=%d leaves=%d acceptance=%v",
		clip(o.tree.Goal, 60), goalIsRunnable(o.tree.Goal), root.State,
		len(root.ChildrenIDs), len(o.tree.Leaves()), root.DoD.Commands)

	var blockers []string
	for _, miss := range o.uncoveredGoalPaths(root) {
		blockers = append(blockers, fmt.Sprintf(
			"goal acceptance references %q but no leaf declares it as an output", miss))
	}
	return blockers
}

// entryPointNames lists the file names that count as an executable entry for
// the detected toolchain (same table treeHasEntryPoint uses).
func (o *Orchestrator) entryPointNames() []string {
	switch o.projectType() {
	case tools.ProjectNode:
		return []string{"index.js", "server.js", "app.js", "main.js"}
	case tools.ProjectPython:
		return []string{"main.py", "app.py", "server.py"}
	case tools.ProjectRust:
		return []string{"main.rs"}
	default:
		return []string{"main.go"}
	}
}

// allOutputs collects every declared output in the tree, normalized.
func (o *Orchestrator) allOutputs() []string {
	var out []string
	for _, leaf := range o.tree.Leaves() {
		for _, o := range leaf.Contract.Outputs {
			if n := engine.NormalizeOutputPath(o); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// uncoveredGoalPaths returns the deliverable paths named by the goal-level
// acceptance that no leaf claims to produce. This is the check that would have
// caught "go run ./cmd/demo" with no cmd/demo task: the acceptance command and
// the task tree are produced by two independent model calls and nothing else
// compares them.
func (o *Orchestrator) uncoveredGoalPaths(root *models.TaskNode) []string {
	paths := goalCommandPaths(root.DoD.Commands)
	if len(paths) == 0 {
		return nil
	}
	outputs := o.allOutputs()
	var missing []string
	for _, p := range paths {
		if !pathCoveredByOutputs(p, outputs) {
			missing = append(missing, p)
		}
	}
	return missing
}

// goalCommandPaths extracts the concrete deliverable paths a command needs to
// exist. Only unambiguous forms are recognized; anything that names a package
// pattern (go test ./..., go build ./...) or the module root yields nothing,
// because a package-wide check cannot point at one missing file and guessing
// would produce false blockers.
func goalCommandPaths(cmds []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`))
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, cmd := range cmds {
		fields := strings.Fields(cmd)
		// Skip leading env assignments: "GOFLAGS=-mod=mod go run ./x".
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		switch {
		case len(fields) >= 3 && fields[0] == "go" && fields[1] == "run":
			if isDeliverablePath(fields[2]) {
				add(fields[2])
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
					add(f)
					break
				}
			}
		case isExecPath(fields[0]):
			if isDeliverablePath(fields[0]) {
				add(fields[0])
			}
		case len(fields) >= 2:
			switch fields[0] {
			case "python", "python3", "node", "deno", "bun", "sh", "bash", "zsh", "pwsh":
				if isDeliverablePath(fields[1]) {
					add(fields[1])
				}
			}
		}
	}
	return out
}

// isExecPath reports whether the command word is a direct path to an
// executable, on either separator convention.
func isExecPath(s string) bool {
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, ".\\") || strings.HasPrefix(s, "..\\") ||
		strings.HasPrefix(s, "/")
}

// isDeliverablePath rejects package patterns and module-root references: they
// describe a scope, not a file that must exist at a known place.
func isDeliverablePath(p string) bool {
	if p == "" || p == "." || p == ".." || p == "..." || p == "./..." {
		return false
	}
	if strings.HasPrefix(p, "-") || strings.Contains(p, "*") {
		return false
	}
	if strings.HasSuffix(p, "/...") {
		return false
	}
	return true
}

// pathCoveredByOutputs reports whether some leaf claims a file inside target
// (or an ancestor directory of it). Coverage is deliberately directory-aware:
// "go run ./cmd/demo" is satisfied by a leaf declaring cmd/demo/main.go.
func pathCoveredByOutputs(target string, outputs []string) bool {
	t := engine.NormalizeOutputPath(target)
	if t == "" {
		return true
	}
	for _, o := range outputs {
		if o == t {
			return true
		}
		// A file inside the target directory.
		if strings.HasPrefix(o, t+"/") {
			return true
		}
		// The target is a file inside a directory the leaf declared.
		if strings.HasPrefix(t, o+"/") {
			return true
		}
	}
	return false
}

// planStructureMarker tags a replan triggered by planBlockers (as opposed to
// an unparseable JSON reply), so the decomposer can tell the model what is
// structurally wrong instead of just asking it to fix its JSON.
const planStructureMarker = "[plan-structure]"

// planIncompleteError signals that the plan parsed fine but cannot deliver the
// goal. It is retried against the planner rather than against a leaf, which is
// why it must not consume the JSON-syntax retry budget.
type planIncompleteError struct {
	blockers []string
}

func (e *planIncompleteError) Error() string {
	return strings.Join(e.blockers, "\n")
}
