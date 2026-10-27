package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDescriptions_CoverEveryToolLine(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []string
	}{
		{"Descriptions", Descriptions(), []string{"- list_dir", "- read_file", "- write_file", "- replace_lines", "- run_bash", "- search_files", "- find_files", "- finish"}},
		{"WebDescriptions", WebDescriptions(), []string{"- web_search", "- web_fetch", "- download_file"}},
		{"HTTPDescriptions", HTTPDescriptions(), []string{"- http_request"}},
		{"CodeDescriptions", CodeDescriptions(), []string{"- find_symbol", "- outline", "- json_query", "- sqlite_query"}},
		{"GitDescriptions", GitDescriptions(), []string{"- git_status", "- git_diff", "- git_log", "- review_diff"}},
		{"BrowserDescriptions", BrowserDescriptions(), []string{"- browser_navigate", "- browser_click", "- browser_type", "- browser_text", "- browser_screenshot"}},
	}
	for _, c := range cases {
		if c.out == "" {
			t.Errorf("%s must not be empty", c.name)
		}
		for _, w := range c.want {
			if !strings.Contains(c.out, w) {
				t.Errorf("%s missing %q:\n%s", c.name, w, c.out)
			}
		}
	}
}

func TestDynamicToolDescriptions_OptionalSections(t *testing.T) {
	plain, _ := NewSandbox(t.TempDir())
	out := plain.DynamicToolDescriptions()
	if !strings.Contains(out, "find_symbol") {
		t.Fatal("code tools are always available")
	}
	if strings.Contains(out, "git_status") {
		t.Fatal("git section must be absent outside a repository")
	}
	if strings.Contains(out, "web_search") {
		t.Fatal("web section must be absent without a WebClient")
	}
	if strings.Contains(out, "browser_navigate") {
		t.Fatal("browser section must be absent without a BrowserClient")
	}

	sb := setupGitRepo(t, map[string]string{"a.txt": "hi"})
	if out = sb.DynamicToolDescriptions(); !strings.Contains(out, "git_status") {
		t.Fatal("git section must appear inside a repository")
	}

	sb.Web = newTestWeb(t, "", true)
	if out = sb.DynamicToolDescriptions(); !strings.Contains(out, "web_search") ||
		!strings.Contains(out, "http_request") {
		t.Fatalf("web+http sections must appear with a WebClient:\n%s", out)
	}

	sb.Browser = &BrowserClient{}
	if out = sb.DynamicToolDescriptions(); !strings.Contains(out, "browser_navigate") {
		t.Fatal("browser section must appear with a BrowserClient")
	}
}

// nativeFuncName extracts the function name of an OpenAI tool definition.
func nativeFuncName(def map[string]any) string {
	fn, ok := def["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

func TestNativeToolDefinitions_Structure(t *testing.T) {
	check := func(t *testing.T, defs []map[string]any, wantNames ...string) {
		t.Helper()
		if len(defs) != len(wantNames) {
			t.Fatalf("got %d defs, want %d", len(defs), len(wantNames))
		}
		for i, def := range defs {
			if def["type"] != "function" {
				t.Fatalf("%s: type must be function", wantNames[i])
			}
			if got := nativeFuncName(def); got != wantNames[i] {
				t.Fatalf("def %d name = %q, want %q", i, got, wantNames[i])
			}
			fn := def["function"].(map[string]any)
			if fn["description"] == "" {
				t.Fatalf("%s: description must not be empty", wantNames[i])
			}
			params, ok := fn["parameters"].(map[string]any)
			if !ok || params["type"] != "object" {
				t.Fatalf("%s: parameters must be an object schema", wantNames[i])
			}
		}
	}

	check(t, NativeTools(),
		ToolListDir, ToolReadFile, ToolWriteFile, ToolReplaceLines, ToolDeletePath,
		ToolMovePath, ToolRunBash, ToolSearchFiles, ToolFindFiles, ToolFinish)
	check(t, NativeWebTools(), ToolWebSearch, ToolWebFetch)
	check(t, NativeCodeTools(), ToolFindSymbol, ToolOutline, ToolJSONQuery, ToolSQLiteQuery)
	check(t, NativeGitTools(), ToolGitStatus, ToolGitDiff, ToolGitLog, ToolReviewDiff)
	check(t, NativeBrowserTools(), ToolBrowserNavigate, ToolBrowserClick, ToolBrowserType, ToolBrowserText, ToolBrowserScreenshot)

	if def := NativeHTTPTool(); nativeFuncName(def) != ToolHTTPRequest {
		t.Fatalf("NativeHTTPTool name = %q", nativeFuncName(def))
	}
	if def := NativeDownloadTool(); nativeFuncName(def) != ToolDownloadFile {
		t.Fatalf("NativeDownloadTool name = %q", nativeFuncName(def))
	}

	// nativeDef shapes: required list and property types propagate.
	def := nativeDef("probe", "desc", map[string]string{"a": "string", "n": "integer"}, []string{"a"})
	params := def["function"].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if props["a"].(map[string]any)["type"] != "string" {
		t.Fatal("property type not propagated")
	}
	req := params["required"].([]string)
	if len(req) != 1 || req[0] != "a" {
		t.Fatalf("required = %v", req)
	}
	if req, ok := NativeGitTools()[0]["function"].(map[string]any)["parameters"].(map[string]any)["required"].([]string); ok && len(req) != 0 {
		t.Fatalf("git_status must have no required args, got %v", req)
	}
}

func TestCall_ArgumentHelpers(t *testing.T) {
	if s, ok := stringArg(nil, "k"); ok || s != "" {
		t.Fatal("nil args must yield empty string")
	}
	if s, ok := stringArg(map[string]any{"n": 42}, "n"); !ok || s != "42" {
		t.Fatalf("numeric arg should JSON-marshal, got %q ok=%v", s, ok)
	}
	if _, ok := stringArg(map[string]any{"m": make(chan int)}, "m"); !ok {
		t.Fatal("unmarshalable values fall back to fmt.Sprint")
	}

	if boolArg(nil, "k", true) != true {
		t.Fatal("nil args must return the default")
	}
	if boolArg(map[string]any{"k": "1"}, "k", false) != true {
		t.Fatal("string \"1\" parses as true")
	}
	if boolArg(map[string]any{"k": "not-a-bool"}, "k", false) != false {
		t.Fatal("unparseable string falls back to the default")
	}

	for _, tc := range []struct {
		v    any
		want int
		ok   bool
	}{
		{7, 7, true},
		{int64(8), 8, true},
		{2.9, 2, true},
		{json.Number("9"), 9, true},
		{"10", 10, true},
		{true, 0, false},
	} {
		got, err := intArg(map[string]any{"k": tc.v}, "k")
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("intArg(%v) = %d, %v", tc.v, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("intArg(%v) should fail", tc.v)
		}
	}
	if _, err := intArg(nil, "k"); err == nil {
		t.Fatal("nil args must error")
	}
	if _, err := intArg(map[string]any{"k": nil}, "k"); err == nil {
		t.Fatal("nil value must error")
	}

	if _, err := stringsArg(map[string]any{"paths": "a.go"}, "paths"); err == nil {
		t.Fatal("non-array paths must error")
	}
	if _, err := stringsArg(map[string]any{"paths": []any{"a.go", 3}}, "paths"); err == nil {
		t.Fatal("non-string entries must error")
	}
	if _, err := stringsArg(map[string]any{}, "paths"); err == nil {
		t.Fatal("missing paths must error")
	}

	if _, err := editsArg(map[string]any{}, "edits"); err == nil {
		t.Fatal("missing edits must error")
	}
	if _, err := editsArg(map[string]any{"edits": "x"}, "edits"); err == nil {
		t.Fatal("non-array edits must error")
	}
	if _, err := editsArg(map[string]any{"edits": []any{"x"}}, "edits"); err == nil {
		t.Fatal("non-object edit entries must error")
	}
	edits, err := editsArg(map[string]any{"edits": []any{map[string]any{"old_string": "a", "new_string": "b"}}}, "edits")
	if err != nil || len(edits) != 1 || edits[0].Old != "a" || edits[0].New != "b" {
		t.Fatalf("valid edit parse failed: %v %v", edits, err)
	}

	if _, err := requireString(map[string]any{"p": ""}, "p"); err == nil {
		t.Fatal("empty string must be rejected by requireString")
	}
}

func TestFormatExec_Variants(t *testing.T) {
	out := formatExec(&ExecResult{Stdout: "hello\n"})
	if !strings.HasPrefix(out, "exit_code: 0\n") || !strings.Contains(out, "stdout:\nhello") {
		t.Fatalf("plain success report wrong:\n%s", out)
	}
	out = formatExec(&ExecResult{ExitCode: 2, Stderr: "boom"})
	if !strings.Contains(out, "exit_code: 2") || !strings.Contains(out, "stderr:\nboom") {
		t.Fatalf("failure report wrong:\n%s", out)
	}
	out = formatExec(&ExecResult{ExitCode: -1, TimedOut: true})
	if !strings.Contains(out, "timed_out: true") || !strings.Contains(out, "(no output)") {
		t.Fatalf("timeout report wrong:\n%s", out)
	}
}

func TestCall_UnknownAndFinish(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	if _, err := sb.Call(ctx, "no_such_tool", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown tool must error, got %v", err)
	}
	out, err := sb.Call(ctx, ToolFinish, nil)
	if err != nil || out != "finished" {
		t.Fatalf("empty finish summary = %q, %v", out, err)
	}
	out, _ = sb.Call(ctx, " FINISH ", map[string]any{"summary": "done"})
	if out != "done" {
		t.Fatalf("name should be trimmed/lowered and summary returned, got %q", out)
	}
}

func TestCall_ArgValidationErrors(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	cases := map[string]map[string]any{
		ToolReadFile:     {"path": ""},
		ToolWriteFile:    {"path": "a.txt"},
		ToolReplaceLines: {"path": "a.txt"},
		ToolDeletePath:   {},
		ToolMovePath:     {"from": "a"},
		ToolRunBash:      {},
		ToolSearchFiles:  {},
		ToolFindFiles:    {},
		ToolWebSearch:    {},
		ToolWebFetch:     {},
		ToolHTTPRequest:  {},
		ToolDownloadFile: {"url": "https://x"},
		ToolFindSymbol:   {},
		ToolOutline:      {},
		ToolJSONQuery:    {},
		ToolSQLiteQuery:  {"path": "d.db"},
	}
	for tool, args := range cases {
		if _, err := sb.Call(ctx, tool, args); err == nil {
			t.Errorf("%s with incomplete args %v must error", tool, args)
		}
	}
}

func TestCall_ReadFileBatchErrors(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	if err := sb.WriteFile("a.txt", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Call(ctx, ToolReadFile, map[string]any{"paths": []any{"a.txt", 5}}); err == nil {
		t.Fatal("non-string batch entry must error")
	}
	if _, err := sb.Call(ctx, ToolReadFile, map[string]any{"paths": "a.txt"}); err == nil {
		t.Fatal("non-array paths must error")
	}
	out, err := sb.Call(ctx, ToolReadFile, map[string]any{"paths": []any{"a.txt"}})
	if err != nil || !strings.Contains(out, "### a.txt") || !strings.Contains(out, "one") {
		t.Fatalf("single-file batch failed: %q %v", out, err)
	}
	// start_line without end_line reads to EOF.
	out, err = sb.Call(ctx, ToolReadFile, map[string]any{"path": "a.txt", "start_line": 1})
	if err != nil || !strings.Contains(out, "1: one") {
		t.Fatalf("ranged read failed: %q %v", out, err)
	}
}

func TestCall_ReplaceLinesVariants(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	if err := sb.WriteFile("a.txt", "one\ntwo\nthree\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Call(ctx, ToolReplaceLines, map[string]any{
		"path": "a.txt", "edits": []any{map[string]any{"old_string": "two", "new_string": "TWO"}},
	}); err != nil {
		t.Fatal(err)
	}
	if content := readFile(t, sb.Root+"/a.txt"); content != "one\nTWO\nthree\n" {
		t.Fatalf("atomic edit wrong: %q", content)
	}
	if _, err := sb.Call(ctx, ToolReplaceLines, map[string]any{
		"path": "a.txt", "edits": []any{map[string]any{"old_string": "two", "new_string": "b"}},
	}); err == nil {
		t.Fatal("edit whose old_string no longer exists must error")
	}
	// old_string/new_string single replacement through Call.
	if _, err := sb.Call(ctx, ToolReplaceLines, map[string]any{
		"path": "a.txt", "old_string": "TWO", "new_string": "deux",
	}); err != nil {
		t.Fatal(err)
	}
	// Line-range replacement with default end (same line) and content.
	if _, err := sb.Call(ctx, ToolReplaceLines, map[string]any{
		"path": "a.txt", "start_line": 1, "content": "premier",
	}); err != nil {
		t.Fatal(err)
	}
	if content := readFile(t, sb.Root+"/a.txt"); !strings.HasPrefix(content, "premier\n") {
		t.Fatalf("line replacement wrong: %q", content)
	}
}

func TestCall_RunBashReport(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	out, err := sb.Call(ctx, ToolRunBash, map[string]any{"command": "printf hi; printf err 1>&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exit_code: 3", "stdout:\nhi", "stderr:\nerr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	// A timeout produces the timed_out marker.
	out, err = sb.Call(ctx, ToolRunBash, map[string]any{"command": "sleep 2", "timeout_sec": 1})
	if err != nil || !strings.Contains(out, "timed_out: true") {
		t.Fatalf("timeout report = %q, %v", out, err)
	}
}

func TestCall_HTTPHeadersNonStringValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Num", r.Header.Get("X-Num"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}
	out, err := sb.Call(context.Background(), ToolHTTPRequest, map[string]any{
		"url":     srv.URL,
		"method":  "get",
		"headers": map[string]any{"X-Num": 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "X-Num: 7") {
		t.Fatalf("non-string header value should be stringified:\n%s", out)
	}
}

func TestBrowserTools_DisabledAcrossAllFive(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	for _, tool := range []string{ToolBrowserNavigate, ToolBrowserClick, ToolBrowserType, ToolBrowserText, ToolBrowserScreenshot} {
		_, err := sb.Call(ctx, tool, map[string]any{"url": "https://x", "selector": "#i", "text": "t", "path": "p.png"})
		if err == nil || !strings.Contains(err.Error(), "-browser") {
			t.Fatalf("%s must report the disabled error, got %v", tool, err)
		}
	}
}

func TestCall_WebSearchVariants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ddgSample))
	}))
	defer srv.Close()

	w := newTestWeb(t, srv.URL+"/?q={query}", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}
	ctx := context.Background()
	// max_results <= 0 falls back to the default cap; trimming the query.
	out, err := sb.Call(ctx, ToolWebSearch, map[string]any{"query": "  go  ", "max_results": 0})
	if err != nil || !strings.Contains(out, "The Go Programming Language") {
		t.Fatalf("search failed: %q %v", out, err)
	}
	// Empty query is rejected before any request.
	if _, err := sb.Call(ctx, ToolWebFetch, map[string]any{"url": "not a url"}); err == nil {
		t.Fatal("invalid fetch URL must error")
	}
}

func TestDownloadFile_SchemeGate(t *testing.T) {
	w := newTestWeb(t, "", true)
	sb := &Sandbox{Root: t.TempDir(), Web: w}
	_, err := sb.Call(context.Background(), ToolDownloadFile, map[string]any{
		"url": "ftp://example.com/x", "path": "x.bin",
	})
	if err == nil || !strings.Contains(err.Error(), "http/https") {
		t.Fatalf("non-http scheme must be rejected, got %v", err)
	}
}

func TestSimpleGlobMatch(t *testing.T) {
	cases := []struct {
		rel, glob string
		want      bool
	}{
		{"pkg/a.go", "*.go", true},
		{"pkg/a.go", "pkg/*.go", true},
		{"pkg/a.go", "a.go", true},
		{"pkg/a.go", "b.go", false},
		{"pkg/a.go", "pkg", false},
		{"main.go", "main.go", true},
	}
	for _, c := range cases {
		if got := simpleGlobMatch(c.rel, c.glob); got != c.want {
			t.Errorf("simpleGlobMatch(%q, %q) = %v, want %v", c.rel, c.glob, got, c.want)
		}
	}
}

func TestSplitJSONPath_Tokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a.b.c", []string{"a", "b", "c"}},
		{"a[0].b", []string{"a", "0", "b"}},
		{`a["x.y"].b`, []string{"a", "x.y", "b"}},
		{`a['q']`, []string{"a", "q"}},
		{"a..b", []string{"a", "b"}},
		{`a["unterminated`, []string{"a", `["unterminated`}},
		{"a[unterminated", []string{"a", "unterminated"}},
	}
	for _, c := range cases {
		got := splitJSONPath(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitJSONPath(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitJSONPath(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestWalkJSONPath_ScalarRootAndBounds(t *testing.T) {
	if _, ok := walkJSONPath("scalar", "a"); ok {
		t.Fatal("scalar root must not resolve any path")
	}
	data := map[string]any{"list": []any{1, 2}}
	if _, ok := walkJSONPath(data, "list[5]"); ok {
		t.Fatal("out-of-range index must not resolve")
	}
	if _, ok := walkJSONPath(data, "list[a]"); ok {
		t.Fatal("non-numeric index must not resolve")
	}
	if _, ok := walkJSONPath(data, "missing"); ok {
		t.Fatal("missing key must not resolve")
	}
	if v, ok := walkJSONPath(data, ""); !ok || v == nil {
		t.Fatal("empty query returns the whole document")
	}
}

func TestJSONQuery_ErrorPaths(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if _, err := sb.JSONQuery("", "a"); err == nil {
		t.Fatal("empty path must error")
	}
	if err := sb.WriteFile("bad.json", "{nope"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.JSONQuery("bad.json", "a"); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid JSON must error, got %v", err)
	}
	if err := sb.WriteFile("ok.json", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.JSONQuery("ok.json", "missing"); err == nil {
		t.Fatal("missing path must error")
	}
	// Non-string values come back JSON-encoded; marshaling never fails here.
	out, err := sb.JSONQuery("ok.json", "a")
	if err != nil || out != "1" {
		t.Fatalf("numeric result = %q, %v", out, err)
	}
}

func TestClipLine_LongLines(t *testing.T) {
	long := strings.Repeat("x", 300)
	if got := clipLine(long); len(got) != maxMatchedLineLen+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long line not clipped (len %d)", len(got))
	}
	if got := clipLine("  spaced  "); got != "spaced" {
		t.Fatalf("short line should be trimmed, got %q", got)
	}
}

func TestTrimPath_AndMergeConflictError(t *testing.T) {
	if got := TrimPath("/tmp/divvy_mirror-abc123/ws"); !strings.HasSuffix(got, "(isolated workspace)") {
		t.Fatalf("mirror path not abbreviated: %q", got)
	}
	if got := TrimPath("/plain/path"); got != "/plain/path" {
		t.Fatalf("plain path changed: %q", got)
	}
	e := &MergeConflictError{Paths: []string{"a.go", "b.go"}}
	msg := e.Error()
	if !strings.Contains(msg, "a.go, b.go") || !strings.Contains(msg, "merge conflict") {
		t.Fatalf("conflict message unexpected: %q", msg)
	}
}

func TestGitReadTools_ErrorPathsAndFilters(t *testing.T) {
	// Outside a repository every read tool fails.
	plain, _ := NewSandbox(t.TempDir())
	ctx := context.Background()
	for name, args := range map[string]map[string]any{
		ToolGitDiff:    {"staged": true, "path": "."},
		ToolGitLog:     {"limit": 3, "path": "."},
		ToolReviewDiff: {"staged": false},
	} {
		if _, err := plain.Call(ctx, name, args); err == nil ||
			!strings.Contains(err.Error(), "not a git repository") {
			t.Fatalf("%s outside repo must fail, got %v", name, err)
		}
	}

	// Inside a repository the path/staged filters reach the git command.
	sb := setupGitRepo(t, map[string]string{"sub/a.txt": "one", "sub/b.txt": "two"})
	if err := sb.WriteFile("sub/a.txt", "changed"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(ctx, ToolGitDiff, map[string]any{"staged": false, "path": "sub/a.txt"})
	if err != nil || !strings.Contains(out, "changed") {
		t.Fatalf("path-filtered diff failed: %q %v", out, err)
	}
	out, err = sb.Call(ctx, ToolGitLog, map[string]any{"limit": 0, "path": "sub"})
	if err != nil || !strings.Contains(out, "baseline") {
		t.Fatalf("path-filtered log failed: %q %v", out, err)
	}
	// limit <= 0 falls back to the default; git output stays usable.
	out, err = sb.Call(ctx, ToolGitLog, nil)
	if err != nil || !strings.Contains(out, "baseline") {
		t.Fatalf("default log failed: %q %v", out, err)
	}
}

func TestOutline_TypeShapesAndVarGroups(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	src := `package pkg

type W struct{ A int }
type I interface{ Do() }
type F func(int) error
type M map[string]int

var (
	One   = 1
	Two   = 2
)
const Three = 3

func Do(ctx context.Context) error { return nil }
`
	if err := sb.WriteFile("t.go", src); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Outline("t.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type W struct", "type I interface", "type F func(int) error",
		"type M map[string]int", "One, Two", "func Do(ctx context.Context) error",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("outline missing %q:\n%s", want, out)
		}
	}
}

func TestClipOutlineText_Long(t *testing.T) {
	got := clipOutlineText(strings.Repeat("a", 200))
	if len(got) != 143 || !strings.HasSuffix(got, "...") {
		t.Fatalf("outline text not clipped to 140+3: %d", len(got))
	}
}
