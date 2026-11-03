package agent

import (
	"regexp"
	"strings"

	"github.com/esrrhs/divvy/pkg/models"
)

// pathLikeToken matches relative file paths that look like deliverables
// (pkg/foo.go, src/main.cpp, CMakeLists.txt, …). Used to backfill empty
// contract.outputs so plan checks and parallel mutexes have something to claim.
var pathLikeToken = regexp.MustCompile(`(?i)(?:^|[\s"'` + "`" + `(=])((?:[\w.-]+/)*[\w.-]+\.(?:go|py|rs|js|jsx|ts|tsx|c|cc|cpp|cxx|h|hpp|java|kt|cs|md|txt|json|yml|yaml|toml|cmake)|CMakeLists\.txt)`)

// ensureOutputs backfills contract.outputs when the planner left them empty.
// Empty outputs cause false "no entry point" plan warnings and disable the
// parallel output mutex (every leaf claims nothing, so everything races).
// Prefer concrete paths extracted from the description/title; fall back to a
// stable per-leaf placeholder so the mutex still serializes siblings.
func ensureOutputs(c *models.ContractSpec, id, title, description string) {
	if c == nil {
		return
	}
	var kept []string
	seen := map[string]bool{}
	for _, o := range c.Outputs {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		kept = append(kept, o)
	}
	c.Outputs = kept
	if len(c.Outputs) > 0 {
		return
	}
	blob := title + "\n" + description
	for _, m := range pathLikeToken.FindAllStringSubmatch(blob, -1) {
		if len(m) < 2 {
			continue
		}
		p := strings.TrimSpace(m[1])
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		c.Outputs = append(c.Outputs, p)
	}
	if len(c.Outputs) > 0 {
		return
	}
	base := sanitizeID(id)
	if base == "" || base == "task" {
		base = sanitizeID(title)
	}
	if base == "" || base == "task" {
		base = "leaf"
	}
	c.Outputs = []string{base + ".out"}
}
