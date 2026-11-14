package tools

import "strings"

// Tool-name tolerance
//
// Weak models do not read the tool list, they recall what tools are called in
// other agents. The first real bench run on a 27B local model opened with
// `batch_read` — a name that does not exist here, although read_file accepts
// the same `paths` batch — and paid a whole model round trip (60s on a
// laptop, real money on an API) to be told so.
//
// So a small, deliberately unambiguous alias table maps those names onto the
// real tool before dispatch. What is *not* here matters as much as what is:
// "search" is absent (file search or web search?), and so is "stop" (stop the
// run, or finish the turn?). An alias that guesses wrong is worse than an
// error, because the model never learns it guessed.

// knownToolNames is every tool the sandbox can dispatch. It drives both the
// unknown-tool message and the "did you mean" suggestion.
var knownToolNames = []string{
	ToolListDir, ToolReadFile, ToolWriteFile, ToolReplaceLines,
	ToolDeletePath, ToolMovePath, ToolRunBash,
	ToolSearchFiles, ToolFindFiles, ToolOutline, ToolFindSymbol,
	ToolWebSearch, ToolWebFetch, ToolDownloadFile, ToolHTTPRequest,
	ToolGitStatus, ToolGitDiff, ToolGitLog, ToolReviewDiff,
	ToolJSONQuery, ToolSQLiteQuery,
	ToolBrowserNavigate, ToolBrowserClick, ToolBrowserType, ToolBrowserText, ToolBrowserScreenshot,
	ToolFinish,
}

// toolAliases maps names models reach for onto the tool that means the same
// thing. Every entry must have exactly one plausible meaning.
var toolAliases = map[string]string{
	"read": ToolReadFile, "read_files": ToolReadFile, "batch_read": ToolReadFile,
	"cat": ToolReadFile, "view": ToolReadFile, "open": ToolReadFile,

	"write": ToolWriteFile, "create_file": ToolWriteFile, "save_file": ToolWriteFile,

	"edit": ToolReplaceLines, "patch": ToolReplaceLines, "replace": ToolReplaceLines,
	"apply_edit": ToolReplaceLines, "str_replace": ToolReplaceLines,

	"bash": ToolRunBash, "shell": ToolRunBash, "exec": ToolRunBash,
	"run_command": ToolRunBash, "terminal": ToolRunBash,

	"grep": ToolSearchFiles, "ripgrep": ToolSearchFiles, "search_code": ToolSearchFiles,
	"glob": ToolFindFiles, "find": ToolFindFiles,

	"ls": ToolListDir, "list": ToolListDir, "list_files": ToolListDir,

	"rm": ToolDeletePath, "delete": ToolDeletePath, "remove_file": ToolDeletePath,

	"mv": ToolMovePath, "rename": ToolMovePath, "move": ToolMovePath,

	"done": ToolFinish, "complete": ToolFinish, "task_done": ToolFinish,
}

// CanonicalToolName resolves an invented-but-unambiguous tool name to the real
// one. Canonical names, MCP-owned names and anything unknown pass through
// unchanged (lowercased), so callers may apply it unconditionally.
func CanonicalToolName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := toolAliases[n]; ok {
		return canonical
	}
	return n
}

// suggestTool returns the closest real tool name, or "" when nothing is close
// enough. The threshold requires the typo to be at most a third of the name:
// "reed_file" qualifies, "xyz" does not.
func suggestTool(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	best, bestDist := "", len(n)
	for _, cand := range knownToolNames {
		d := editDistance(n, cand)
		if d < bestDist {
			best, bestDist = cand, d
		}
	}
	if best == "" || bestDist > 2 || bestDist*3 > len(n) {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, capped implicitly
// by the short strings involved (tool names, not documents).
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func minInt(vals ...int) int {
	out := vals[0]
	for _, v := range vals[1:] {
		if v < out {
			out = v
		}
	}
	return out
}
