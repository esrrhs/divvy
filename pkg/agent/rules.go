package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/esrrhs/divvy/pkg/tools"
)

// Project rule files are the repository's standing instructions to coding
// agents. Cursor calls these rules; AGENTS.md is the cross-tool equivalent
// already used by other agents, so a project migrating to divvy keeps its
// guidance without duplication.
//
// The whole block is bounded: rules are context, not a second goal, and a
// generated/vendored file must not crowd out the actual task.
const projectRulesMaxBytes = 16 * 1024

// projectRulesCandidates are read in order. The first match wins per path, and
// duplicate content is emitted only once so `AGENTS.md` symlinked/copied to
// `DIVVY.md` does not double the prompt.
var projectRulesCandidates = []string{
	"AGENTS.md",
	"DIVVY.md",
	".divvy/rules.md",
}

// loadProjectRules returns the rendered project-rules block for the sandbox,
// or "" when the project ships none. Read/parse failures fail open: missing or
// unreadable rules must not prevent a run.
func loadProjectRules(sb *tools.Sandbox) string {
	if sb == nil {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}
	total := 0
	truncated := false
	sources := make([]string, 0, 4)

	add := func(path string) {
		if truncated {
			return
		}
		body, err := sb.ReadFile(path)
		if err != nil {
			return
		}
		body = strings.TrimSpace(body)
		if body == "" || seen[body] {
			return
		}
		remaining := projectRulesMaxBytes - total
		if remaining <= 0 {
			truncated = true
			return
		}
		if len(body) > remaining {
			body = body[:remaining] + "\n\n[project rules truncated at " + strconv.Itoa(projectRulesMaxBytes) + " bytes]"
			truncated = true
		}
		seen[body] = true
		sources = append(sources, path)
		b.WriteString("\n\n### ")
		b.WriteString(path)
		b.WriteString("\n")
		b.WriteString(body)
		total += len(body)
	}

	for _, path := range projectRulesCandidates {
		add(path)
	}
	addProjectRuleDir(sb, add)

	if strings.TrimSpace(b.String()) == "" {
		return ""
	}
	return "PROJECT RULES\nThe repository ships standing instructions for coding agents (files below). Treat them as binding constraints for this run, alongside the user's request; an explicit user request wins on conflict. Follow them without spending tool calls re-reading the files.\n" +
		strings.TrimSpace(b.String())
}

// addProjectRuleDir adds .divvy/rules/*.md in lexical order. Directory listing
// is best-effort: a project without the directory is the common case.
func addProjectRuleDir(sb *tools.Sandbox, add func(string)) {
	dir := filepath.Join(sb.Root, ".divvy", "rules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(filepath.ToSlash(filepath.Join(".divvy", "rules", name)))
	}
}

// projectRulesBlock is the prompt suffix shared by every loop. Workers run
// inside an isolated mirror, which is a snapshot of the same workspace, so the
// rules are identical to what the user sees.
func projectRulesBlock(sb *tools.Sandbox) string {
	rules := loadProjectRules(sb)
	if rules == "" {
		return ""
	}
	return "\n\n" + rules
}
