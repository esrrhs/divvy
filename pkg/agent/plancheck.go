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
	return warns
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
