package agent

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/esrrhs/divvy/pkg/tools"
)

// Rule files are the standing instructions a user or repository gives coding
// agents. Cursor separates user rules from project rules; AGENTS.md is the
// cross-tool project equivalent, so a project migrating to divvy keeps its
// guidance without duplication.
//
// The whole block is bounded: rules are context, not a second goal, and a
// generated/vendored file must not crowd out the actual task.
const projectRulesMaxBytes = 16 * 1024

// userRulesName is read from the data directory, so it applies to every
// project this user opens rather than living inside one repository.
const userRulesName = "rules.md"

// projectRulesCandidates are read in order. Duplicate content is emitted only
// once so `AGENTS.md` symlinked/copied to `DIVVY.md` does not double the
// prompt.
var projectRulesCandidates = []string{
	"AGENTS.md",
	"DIVVY.md",
	".divvy/rules.md",
}

// loadProjectRules returns the rendered rules block for the sandbox, or ""
// when neither the user nor the project ships any. Read/parse failures fail
// open: missing or unreadable rules must not prevent a run.
func loadProjectRules(sb *tools.Sandbox, dataDir string) string {
	if sb == nil {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}
	total := 0
	truncated := false

	add := func(label string, body string, err error) {
		if truncated || err != nil {
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
			body = body[:remaining] + "\n\n[rules truncated at " + strconv.Itoa(projectRulesMaxBytes) + " bytes]"
			truncated = true
		}
		seen[body] = true
		b.WriteString("\n\n### ")
		b.WriteString(label)
		b.WriteString("\n")
		b.WriteString(body)
		total += len(body)
	}

	// User rules apply everywhere; project rules can then specialize them.
	if dataDir != "" {
		body, err := readCapped(filepath.Join(dataDir, userRulesName), projectRulesMaxBytes)
		add(userRulesName+" (user)", body, err)
	}
	// A workspace opened as a subdirectory still belongs to its repository:
	// the repo-root rules are the project's broadest instructions.
	if repoRoot := gitRepoRoot(sb.Root); repoRoot != "" && !samePath(repoRoot, sb.Root) {
		for _, name := range []string{"AGENTS.md", "DIVVY.md"} {
			body, err := readCapped(filepath.Join(repoRoot, name), projectRulesMaxBytes)
			add(name+" (repository root)", body, err)
		}
	}

	for _, path := range projectRulesCandidates {
		body, err := sb.ReadFile(path)
		add(path, body, err)
	}
	addProjectRuleDir(sb, func(path string) {
		body, err := sb.ReadFile(path)
		add(path, body, err)
	})

	if strings.TrimSpace(b.String()) == "" {
		return ""
	}
	return "PROJECT RULES\nStanding instructions for coding agents are listed below (user rules, then project rules). Treat them as binding constraints for this run, alongside the user's request; an explicit user request wins on conflict. Follow them without spending tool calls re-reading the files.\n" +
		strings.TrimSpace(b.String())
}

// readCapped reads a rules file outside the sandbox (user data dir or repo
// root) with the same text/size discipline as Sandbox.ReadFile.
func readCapped(path string, max int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", os.ErrInvalid
	}
	if len(data) > max {
		return string(data[:max]) + "\n\n[rules truncated at " + strconv.Itoa(max) + " bytes]", nil
	}
	return string(data), nil
}

// gitRepoRoot reports the repository root for a workspace, so a project opened
// at a package subdirectory still sees the root AGENTS.md. It returns "" when
// the workspace is not in a git repository.
func gitRepoRoot(root string) string {
	if !tools.IsRepo(root) {
		return ""
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	out, err := exec.Command(git, "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, ea := filepath.Abs(a)
	cb, eb := filepath.Abs(b)
	return ea == nil && eb == nil && ca == cb
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
func projectRulesBlock(sb *tools.Sandbox, dataDir string) string {
	rules := loadProjectRules(sb, dataDir)
	if rules == "" {
		return ""
	}
	return "\n\n" + rules
}
