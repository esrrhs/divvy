package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Names of tools exposed to the model.
const (
	ToolListDir      = "list_dir"
	ToolReadFile     = "read_file"
	ToolWriteFile    = "write_file"
	ToolReplaceLines = "replace_lines"
	ToolRunBash      = "run_bash"
	ToolSearchFiles  = "search_files"
	ToolFinish       = "finish"
)

// Descriptions returns a compact tool list for JSON-mode system prompts.
func Descriptions() string {
	return strings.TrimSpace(`
Tools (call exactly one per turn):
- list_dir: {"path":".","recursive":true}
- read_file: {"path":"file.go"}
- write_file: {"path":"file.go","content":"...full file..."}
- replace_lines: {"path":"file.go","start_line":1,"end_line":3,"content":"new lines"}
  alternative: {"path":"file.go","old_string":"exact old","new_string":"exact new"}
- run_bash: {"command":"go test ./..."}
- search_files: {"pattern":"func Add","glob":"*.go"}
- finish: {"summary":"what was done"}
`)
}

// NativeTools returns OpenAI tool-calling definitions (without finish; finish is JSON-only or a tool).
func NativeTools() []map[string]any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	obj := func(props map[string]any, req []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	fn := func(name, desc string, params map[string]any) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		}
	}
	return []map[string]any{
		fn(ToolListDir, "List files in a workspace directory.", obj(map[string]any{
			"path":      str("Relative directory path"),
			"recursive": map[string]any{"type": "boolean"},
		}, []string{"path"})),
		fn(ToolReadFile, "Read a UTF-8 text file.", obj(map[string]any{
			"path": str("Relative file path"),
		}, []string{"path"})),
		fn(ToolWriteFile, "Create or overwrite a whole file.", obj(map[string]any{
			"path":    str("Relative file path"),
			"content": str("Full file contents"),
		}, []string{"path", "content"})),
		fn(ToolReplaceLines, "Replace a line range or an exact string in a file.", obj(map[string]any{
			"path":        str("Relative file path"),
			"start_line":  map[string]any{"type": "integer"},
			"end_line":    map[string]any{"type": "integer"},
			"content":     str("Replacement lines"),
			"old_string":  str("Exact text to find"),
			"new_string":  str("Replacement text"),
			"replace_all": map[string]any{"type": "boolean"},
		}, []string{"path"})),
		fn(ToolRunBash, "Run a shell command in the workspace.", obj(map[string]any{
			"command": str("Shell command"),
		}, []string{"command"})),
		fn(ToolSearchFiles, "Search file contents with a regular expression and report file:line matches.", obj(map[string]any{
			"pattern":        str("Regular expression to find"),
			"path":           str("Workspace-relative directory to search (default: workspace root)"),
			"glob":           str("Optional glob filter, e.g. *.go or pkg/*_test.go"),
			"case_sensitive": map[string]any{"type": "boolean"},
		}, []string{"pattern"})),
		fn(ToolFinish, "Mark the atomic task complete.", obj(map[string]any{
			"summary": str("Short summary of what was done"),
		}, []string{})),
	}
}

// Call executes a named tool. Returns a string result for the model.
func (s *Sandbox) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case ToolListDir:
		path, _ := stringArg(args, "path")
		if path == "" {
			path = "."
		}
		rec := boolArg(args, "recursive", true)
		out, err := s.ListDir(path, rec)
		if err != nil {
			return "", err
		}
		return out, nil

	case ToolReadFile:
		path, err := requireString(args, "path")
		if err != nil {
			return "", err
		}
		return s.ReadFile(path)

	case ToolWriteFile:
		path, err := requireString(args, "path")
		if err != nil {
			return "", err
		}
		content, err := requireString(args, "content")
		if err != nil {
			return "", err
		}
		if err := s.WriteFile(path, content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), nil

	case ToolReplaceLines:
		path, err := requireString(args, "path")
		if err != nil {
			return "", err
		}
		if old, ok := stringArg(args, "old_string"); ok && old != "" {
			neu, _ := stringArg(args, "new_string")
			all := boolArg(args, "replace_all", false)
			if err := s.ReplaceText(path, old, neu, all); err != nil {
				return "", err
			}
			return fmt.Sprintf("replaced text in %s", path), nil
		}
		start, err := intArg(args, "start_line")
		if err != nil {
			return "", fmt.Errorf("replace_lines needs start_line/end_line or old_string/new_string")
		}
		end, err := intArg(args, "end_line")
		if err != nil {
			end = start
		}
		content, _ := stringArg(args, "content")
		if err := s.ReplaceLines(path, start, end, content); err != nil {
			return "", err
		}
		return fmt.Sprintf("replaced lines %d-%d in %s", start, end, path), nil

	case ToolRunBash:
		command, err := requireString(args, "command")
		if err != nil {
			return "", err
		}
		timeout := time.Duration(0)
		if sec, err := intArg(args, "timeout_sec"); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
		res, err := s.RunBash(ctx, command, timeout)
		if err != nil {
			return "", err
		}
		return formatExec(res), nil

	case ToolSearchFiles:
		pattern, err := requireString(args, "pattern")
		if err != nil {
			return "", err
		}
		rootDir, _ := stringArg(args, "path")
		glob, _ := stringArg(args, "glob")
		caseSensitive := boolArg(args, "case_sensitive", false)
		return s.SearchFiles(pattern, rootDir, glob, caseSensitive)

	case ToolFinish:
		summary, _ := stringArg(args, "summary")
		if summary == "" {
			summary = "finished"
		}
		return summary, nil

	default:
		return "", fmt.Errorf("unknown tool %q; use list_dir, read_file, write_file, replace_lines, run_bash, search_files, finish", name)
	}
}

func formatExec(res *ExecResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if res.TimedOut {
		b.WriteString("timed_out: true\n")
	}
	if res.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if res.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	if res.Stdout == "" && res.Stderr == "" {
		b.WriteString("(no output)\n")
	}
	return strings.TrimSpace(b.String())
}

func requireString(args map[string]any, key string) (string, error) {
	s, ok := stringArg(args, key)
	if !ok || s == "" {
		return "", fmt.Errorf("missing %s", key)
	}
	return s, nil
}

func stringArg(args map[string]any, key string) (string, bool) {
	if args == nil {
		return "", false
	}
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t), true
		}
		return string(b), true
	}
}

func boolArg(args map[string]any, key string, def bool) bool {
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(t)
		if err == nil {
			return b
		}
	}
	return def
}

func intArg(args map[string]any, key string) (int, error) {
	if args == nil {
		return 0, fmt.Errorf("missing %s", key)
	}
	v, ok := args[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("missing %s", key)
	}
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	case json.Number:
		i, err := t.Int64()
		return int(i), err
	case string:
		return strconv.Atoi(t)
	default:
		return 0, fmt.Errorf("invalid %s", key)
	}
}
