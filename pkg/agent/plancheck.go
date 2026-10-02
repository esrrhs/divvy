package agent

import (
	"fmt"
	"strings"

	"github.com/esrrhs/go_llm_engine/pkg/tools"
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
