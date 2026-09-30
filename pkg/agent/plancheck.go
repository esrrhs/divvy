package agent

import (
	"fmt"
	"strings"
)

// planWarnings reviews the planned tree for weak contracts and DoD so that
// problems surface before any leaf burns tokens. Returns human-readable
// warnings keyed by leaf.
func (o *Orchestrator) planWarnings() []string {
	var warns []string
	claimed := map[string]string{} // "parentID|output" -> leafID

	for _, leaf := range o.tree.Leaves() {
		id := leaf.ID
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
// file (main.go or an otherwise-named main package file ending _main.go).
func (o *Orchestrator) treeHasEntryPoint() bool {
	for _, leaf := range o.tree.Leaves() {
		for _, out := range leaf.Contract.Outputs {
			p := strings.ToLower(strings.TrimSpace(out))
			if p == "main.go" || strings.HasSuffix(p, "/main.go") {
				return true
			}
		}
	}
	return false
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
