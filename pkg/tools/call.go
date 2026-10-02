package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	ToolWebSearch    = "web_search"
	ToolWebFetch     = "web_fetch"
	ToolFinish       = "finish"

	ToolHTTPRequest = "http_request"

	ToolGitStatus = "git_status"
	ToolGitDiff   = "git_diff"
	ToolGitLog    = "git_log"

	ToolFindSymbol = "find_symbol"
	ToolJSONQuery  = "json_query"

	ToolBrowserNavigate   = "browser_navigate"
	ToolBrowserClick      = "browser_click"
	ToolBrowserType       = "browser_type"
	ToolBrowserText       = "browser_text"
	ToolBrowserScreenshot = "browser_screenshot"
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

// WebDescriptions returns the extra tool lines exposed when -web is on.
func WebDescriptions() string {
	return strings.TrimSpace(`
- web_search: {"query":"latest docs for ...","max_results":5}  (live web search)
- web_fetch: {"url":"https://example.com/doc"}  (download a page as plain text)
`)
}

// HTTPDescriptions returns the structured HTTP request tool line.
func HTTPDescriptions() string {
	return strings.TrimSpace(`- http_request: {"url":"https://...","method":"GET","headers":{},"body":""}  (returns status/headers/body)`)
}

// CodeDescriptions returns the code/data understanding tool lines.
func CodeDescriptions() string {
	return strings.TrimSpace(`
- find_symbol: {"name":"Foo","kind":"func|type|var","glob":"*.go"}  (definition locations; Go uses a real AST)
- json_query: {"path":"file.json","query":"items.0.name"}  (extract one value via dotted/bracket path)
`)
}

// GitDescriptions returns the read-only git tool lines.
func GitDescriptions() string {
	return strings.TrimSpace(`
- git_status: {}  (short working-tree status)
- git_diff: {"staged":false,"path":""}  (unified diff)
- git_log: {"limit":10,"path":""}  (recent commits)
`)
}

// BrowserDescriptions returns the headless browser tool lines.
func BrowserDescriptions() string {
	return strings.TrimSpace(`
- browser_navigate: {"url":"https://..."}  (JS-rendered page → title + text)
- browser_click: {"selector":"#id"}  (CSS selector)
- browser_type: {"selector":"input","text":"...","clear":true}
- browser_text: {"selector":""}  (rendered text of a selector or body)
- browser_screenshot: {"path":"shot.png"}  (full-page PNG into the workspace)
`)
}

// DynamicToolDescriptions assembles every optional tool line available in
// this sandbox; used to build the worker prompt.
func (s *Sandbox) DynamicToolDescriptions() string {
	var b strings.Builder
	write := func(text string) { b.WriteString("\n" + text + "\n") }

	write(CodeDescriptions())
	if IsRepo(s.Root) {
		write(GitDescriptions())
	}
	if s.Web != nil {
		write(WebDescriptions())
		write(HTTPDescriptions())
	}
	if s.Browser != nil {
		write(BrowserDescriptions())
	}
	return b.String()
}

// nativeDef is a compact builder for OpenAI function tool definitions.
// props maps argument name → JSON schema type ("string"/"integer"/"boolean");
// required lists mandatory arguments.
func nativeDef(name, description string, props map[string]string, required []string) map[string]any {
	propMap := make(map[string]any, len(props))
	for p, typ := range props {
		propMap[p] = map[string]any{"type": typ}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters": map[string]any{
				"type":       "object",
				"properties": propMap,
				"required":   required,
			},
		},
	}
}

// NativeCodeTools returns find_symbol/json_query definitions.
func NativeCodeTools() []map[string]any {
	return []map[string]any{
		nativeDef(ToolFindSymbol, "Locate definitions by name; Go uses the real AST.",
			map[string]string{"name": "string", "kind": "string", "glob": "string"}, []string{"name"}),
		nativeDef(ToolJSONQuery, "Extract one value from a JSON file by a dotted/bracket path.",
			map[string]string{"path": "string", "query": "string"}, []string{"path"}),
	}
}

// NativeGitTools returns read-only git definitions.
func NativeGitTools() []map[string]any {
	return []map[string]any{
		nativeDef(ToolGitStatus, "Show short git status.", map[string]string{}, nil),
		nativeDef(ToolGitDiff, "Show a unified diff.",
			map[string]string{"staged": "boolean", "path": "string"}, nil),
		nativeDef(ToolGitLog, "Show recent oneline commits.",
			map[string]string{"limit": "integer", "path": "string"}, nil),
	}
}

// NativeHTTPTool returns the structured request definition.
func NativeHTTPTool() map[string]any {
	return nativeDef(ToolHTTPRequest, "Perform a structured HTTP call; returns status/headers/body.",
		map[string]string{"url": "string", "method": "string", "headers": "object", "body": "string"},
		[]string{"url"})
}

// NativeBrowserTools returns headless Chrome definitions.
func NativeBrowserTools() []map[string]any {
	return []map[string]any{
		nativeDef(ToolBrowserNavigate, "Load a JS-rendered page; returns title and text.",
			map[string]string{"url": "string"}, []string{"url"}),
		nativeDef(ToolBrowserClick, "Click an element by CSS selector.",
			map[string]string{"selector": "string"}, []string{"selector"}),
		nativeDef(ToolBrowserType, "Type text into a field.",
			map[string]string{"selector": "string", "text": "string", "clear": "boolean"},
			[]string{"selector"}),
		nativeDef(ToolBrowserText, "Read rendered text of a selector or body.",
			map[string]string{"selector": "string"}, nil),
		nativeDef(ToolBrowserScreenshot, "Save a full-page PNG into the workspace.",
			map[string]string{"path": "string"}, []string{"path"}),
	}
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

// NativeWebTools returns the OpenAI tool-calling definitions for the live
// web tools, appended to NativeTools when -web is enabled.
func NativeWebTools() []map[string]any {
	fn := func(name, desc string, props map[string]any, req []string) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
					"required":   req,
				},
			},
		}
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []map[string]any{
		fn(ToolWebSearch, "Search the live web and return numbered title/url/snippet results.",
			map[string]any{"query": str("Search query"), "max_results": map[string]any{"type": "integer"}},
			[]string{"query"}),
		fn(ToolWebFetch, "Fetch a public http(s) URL and return its plain-text content.",
			map[string]any{"url": str("Absolute http or https URL")},
			[]string{"url"}),
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

	case ToolWebSearch:
		query, err := requireString(args, "query")
		if err != nil {
			return "", err
		}
		if s.Web == nil {
			return "", fmt.Errorf("web tools are disabled; restart with -web to enable web_search/web_fetch")
		}
		max := 0
		if n, err := intArg(args, "max_results"); err == nil {
			max = n
		}
		return s.Web.Search(ctx, query, max)

	case ToolWebFetch:
		rawURL, err := requireString(args, "url")
		if err != nil {
			return "", err
		}
		if s.Web == nil {
			return "", fmt.Errorf("web tools are disabled; restart with -web to enable web_search/web_fetch")
		}
		return s.Web.Fetch(ctx, rawURL)

	case ToolHTTPRequest:
		if s.Web == nil {
			return "", fmt.Errorf("http_request needs -web (network access is off by default)")
		}
		u, err := requireString(args, "url")
		if err != nil {
			return "", err
		}
		method, _ := stringArg(args, "method")
		body, _ := stringArg(args, "body")
		headers := map[string]string{}
		if hm, ok := args["headers"].(map[string]any); ok {
			for k, v := range hm {
				if vs, ok := v.(string); ok {
					headers[k] = vs
				} else {
					headers[k] = fmt.Sprint(v)
				}
			}
		}
		return s.Web.Do(ctx, method, u, headers, body)

	case ToolGitStatus:
		return s.GitStatus(ctx)
	case ToolGitDiff:
		staged := boolArg(args, "staged", false)
		p, _ := stringArg(args, "path")
		return s.GitDiff(ctx, staged, p)
	case ToolGitLog:
		limit := 0
		if n, err := intArg(args, "limit"); err == nil {
			limit = n
		}
		p, _ := stringArg(args, "path")
		return s.GitLog(ctx, limit, p)

	case ToolFindSymbol:
		n, err := requireString(args, "name")
		if err != nil {
			return "", err
		}
		kind, _ := stringArg(args, "kind")
		glob, _ := stringArg(args, "glob")
		return s.FindSymbol(ctx, n, kind, glob)

	case ToolJSONQuery:
		p, err := requireString(args, "path")
		if err != nil {
			return "", err
		}
		query, _ := stringArg(args, "query")
		return s.JSONQuery(p, query)

	case ToolBrowserNavigate:
		if s.Browser == nil {
			return "", browserDisabledErr
		}
		u, err := requireString(args, "url")
		if err != nil {
			return "", err
		}
		return s.Browser.Navigate(ctx, u)
	case ToolBrowserClick:
		if s.Browser == nil {
			return "", browserDisabledErr
		}
		sel, err := requireString(args, "selector")
		if err != nil {
			return "", err
		}
		return reportOK(s.Browser.Click(ctx, sel), "clicked "+sel)
	case ToolBrowserType:
		if s.Browser == nil {
			return "", browserDisabledErr
		}
		sel, err := requireString(args, "selector")
		if err != nil {
			return "", err
		}
		text, _ := stringArg(args, "text")
		clear := boolArg(args, "clear", true)
		return reportOK(s.Browser.Type(ctx, sel, text, clear), "typed into "+sel)
	case ToolBrowserText:
		if s.Browser == nil {
			return "", browserDisabledErr
		}
		sel, _ := stringArg(args, "selector")
		return s.Browser.Text(ctx, sel)
	case ToolBrowserScreenshot:
		if s.Browser == nil {
			return "", browserDisabledErr
		}
		out, err := requireString(args, "path")
		if err != nil {
			return "", err
		}
		abs, err := s.Resolve(out)
		if err != nil {
			return "", err
		}
		png, err := s.Browser.Screenshot(ctx)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return "", err
		}
		if err := writeBinary(abs, png); err != nil {
			return "", err
		}
		return fmt.Sprintf("saved %s (%d bytes PNG)", out, len(png)), nil

	case ToolFinish:
		summary, _ := stringArg(args, "summary")
		if summary == "" {
			summary = "finished"
		}
		return summary, nil

	default:
		return "", fmt.Errorf("unknown tool %q; use list_dir, read_file, write_file, replace_lines, run_bash, search_files, web_search, web_fetch, finish", name)
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

// browserDisabledErr is the common error when browser tools are called
// without -browser.
var browserDisabledErr = fmt.Errorf("browser tools are disabled; restart with -browser (requires Chrome/Chromium)")

// reportOK returns okText when err is nil, otherwise the error.
func reportOK(err error, okText string) (string, error) {
	if err != nil {
		return "", err
	}
	return okText, nil
}

func writeBinary(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
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
