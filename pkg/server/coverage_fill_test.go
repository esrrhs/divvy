package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
)

// covAPIServer is apiServerWithClient that also returns the *Server so the
// tests can reach s.dataDir / call handlers directly.
func covAPIServer(t *testing.T, work, data string) (*Server, *httptest.Server) {
	t.Helper()
	return covAPIServerWithClient(t, work, data, func(agent.Config) llm.Client { return srvScript() })
}

func covAPIServerWithClient(t *testing.T, work, data string,
	factory func(agent.Config) llm.Client) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(agent.NewRunManager(), data,
		WithToken("test-token"), WithHeartbeat(50*time.Millisecond),
		WithWorkdir(work), WithClientFactory(factory))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// covFailFS refuses every read; it drives the static-handler fallbacks.
type covFailFS struct{}

func (covFailFS) Open(string) (fs.File, error) { return nil, errors.New("no assets") }

// covNoFlushWriter hides httptest.ResponseRecorder's Flush method so
// handleEvents takes the "streaming unsupported" branch.
type covNoFlushWriter struct {
	http.ResponseWriter
}

// covFailWriter serves the first write then fails, so the SSE heartbeat path
// hits a broken client.
type covFailWriter struct {
	w   http.ResponseWriter
	n   int
	err error
}

func (c *covFailWriter) Header() http.Header    { return c.w.Header() }
func (c *covFailWriter) WriteHeader(status int) { c.w.WriteHeader(status) }
func (c *covFailWriter) Write(p []byte) (int, error) {
	c.n++
	if c.n > 1 {
		return 0, c.err
	}
	return c.w.Write(p)
}
func (c *covFailWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// --- server.go edge branches -------------------------------------------------

func TestCovNewServerRejectsNilManager(t *testing.T) {
	if _, err := New(nil, t.TempDir()); err == nil {
		t.Fatal("New without a run manager must fail")
	}
}

func TestCovWithLoggerOption(t *testing.T) {
	lines := make(chan string, 4)
	s, err := New(agent.NewRunManager(), t.TempDir(), WithToken("t"),
		WithLogger(func(format string, args ...any) {
			select {
			case lines <- format:
			default:
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	s.logf("hello %d", 1)
	if got := <-lines; got != "hello %d" {
		t.Fatalf("logger got %q", got)
	}
}

func TestCovHostAllowlistEmptyHost(t *testing.T) {
	if hostAllowlist("") || hostAllowlist("   ") {
		t.Fatal("an empty Host must be rejected")
	}
}

func TestCovBearerTokenVariants(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	if got := bearerToken(req); got != "" {
		t.Fatalf("missing header = %q", got)
	}
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := bearerToken(req); got != "" {
		t.Fatalf("non-bearer header = %q", got)
	}
	req.Header.Set("Authorization", "Bearer  tok  ")
	if got := bearerToken(req); got != "tok" {
		t.Fatalf("bearer with spaces = %q", got)
	}
}

func TestCovStaticAPIPrefixNotFound(t *testing.T) {
	_, ts := newTestServer(t)
	resp := get(t, ts, "/api/definitely-not-a-route?token=test-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown /api path: %d, want 404", resp.StatusCode)
	}
}

func TestCovStaticFallbackWhenAssetsBroken(t *testing.T) {
	_, ts := newTestServer(t, WithToken("test-token"),
		WithHeartbeat(50*time.Millisecond), WithStatic(covFailFS{}))
	resp := get(t, ts, "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("broken static must fall through to 404, got %d", resp.StatusCode)
	}
}

func TestCovStartBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s, err := New(agent.NewRunManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Start(ln.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("binding an occupied port must fail")
	}
}

// --- sessions.go edge branches -----------------------------------------------

func TestCovResumeLiveSessionIsRejected(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	_, ts := covAPIServer(t, work, data)
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))

	status, resp := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/resume?token=test-token", map[string]any{})
	if status != http.StatusConflict && status != http.StatusBadRequest {
		t.Fatalf("resume of a live session: %d %v", status, resp)
	}
}

func TestCovListSessionsStorageError(t *testing.T) {
	skipIfRoot := func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based failure injection does not work as root")
		}
	}
	skipIfRoot(t)
	work := t.TempDir()
	data := t.TempDir()
	_, ts := covAPIServer(t, work, data)
	if err := os.Chmod(data, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(data, 0o755) })

	status, _ := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("unreadable data dir: %d, want 500", status)
	}
	// Deleting with an unreadable data dir also fails (stat error ≠ NotExist).
	status, _ = apiDo(t, http.MethodDelete, apiBase(ts)+"/sessions/sess_x?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("delete with unreadable data dir: %d, want 500", status)
	}
}

func TestCovDeleteSessionStorageBuildError(t *testing.T) {
	tmp := t.TempDir()
	occupy := filepath.Join(tmp, "occupied")
	if err := os.WriteFile(occupy, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(agent.NewRunManager(), filepath.Join(occupy, "sub"), WithToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/sess_x", nil)
	req.SetPathValue("id", "sess_x")
	s.handleDeleteSession(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unusable dataDir: %d, want 500", rec.Code)
	}
}

func TestCovSessionActionsExtra(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	_, ts := covAPIServer(t, work, data)
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))

	// redo is wired like the other actions and answers with the phase.
	status, resp := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/redo?token=test-token", map[string]any{"node_id": "root"})
	if status != http.StatusOK && status != http.StatusConflict && status != http.StatusBadRequest {
		t.Fatalf("redo: %d %v", status, resp)
	}
	// No approval is pending while parked at plan review.
	status, resp = apiDo(t, http.MethodGet,
		apiBase(ts)+"/sessions/"+id+"/approvals/leaf-1?token=test-token", nil)
	if status != http.StatusNotFound {
		t.Fatalf("approval for unknown leaf: %d %v", status, resp)
	}
	// decide on an unknown session is 404; a broken body is 400.
	status, _ = apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/nope/approvals/leaf-1?token=test-token",
		map[string]any{"decision": "approve"})
	if status != http.StatusNotFound {
		t.Fatalf("decide on unknown session: %d", status)
	}
	status, _ = apiDoRaw(t, http.MethodPost,
		apiBase(ts)+"/sessions/"+id+"/approvals/leaf-1?token=test-token", "{not json")
	if status != http.StatusBadRequest {
		t.Fatalf("decide with broken body: %d", status)
	}
}

func TestCovSessionLogErrors(t *testing.T) {
	skipIfRoot := func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based failure injection does not work as root")
		}
	}
	skipIfRoot(t)
	work, data := t.TempDir(), t.TempDir()
	_, ts := covAPIServer(t, work, data)

	// A finished session's log, then made unreadable.
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	apiDo(t, http.MethodPost, apiBase(ts)+"/sessions/"+id+"/plan/approve?token=test-token", map[string]any{})
	waitLivePhase(t, ts, id, string(agent.PhaseDone))
	logPath := filepath.Join(data, "logs", id+".log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o644) })
	status, _ := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"/log?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("unreadable log: %d, want 500", status)
	}
}

func TestCovSessionLogScannerError(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	_, ts := covAPIServer(t, work, data)
	// A log with one oversized line trips the scanner ceiling.
	id := "sess_biglog"
	if err := os.MkdirAll(filepath.Join(data, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 5*1024*1024)
	if err := os.WriteFile(filepath.Join(data, "logs", id+".log"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _ := apiDo(t, http.MethodGet, apiBase(ts)+"/sessions/"+id+"/log?token=test-token", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("oversized log line: %d, want 500", status)
	}
}

func TestCovDecodeBodyNilReader(t *testing.T) {
	var out map[string]any
	if err := decodeBody(&http.Request{}, &out); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("nil body = %v", err)
	}
}

// --- fs.go edge branches -----------------------------------------------------

func TestCovFSListSessionResolution(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	work2 := t.TempDir()
	s, ts := covAPIServer(t, work, data)
	if err := os.WriteFile(filepath.Join(work2, "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unknown session → 404.
	status, body := fsGet(t, ts.URL+"/api/fs/list?token=test-token&session=nope")
	if status != http.StatusNotFound {
		t.Fatalf("unknown session list: %d %s", status, body)
	}

	// Persisted tree with a WorkDir resolves to that workdir.
	st, err := engine.NewStorage(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	tree := engine.NewTaskTree("sess_fsdirs", "root", "root")
	tree.WorkDir = work2
	if err := st.SaveTree(tree); err != nil {
		t.Fatal(err)
	}
	status, body = fsGet(t, ts.URL+"/api/fs/list?token=test-token&session=sess_fsdirs")
	if status != http.StatusOK || !strings.Contains(string(body), "inner.txt") {
		t.Fatalf("persisted session list: %d %s", status, body)
	}
	// Same for the file API on a persisted session.
	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&session=sess_fsdirs&path=inner.txt")
	if status != http.StatusOK || !strings.Contains(string(body), "inner") {
		t.Fatalf("persisted session file: %d %s", status, body)
	}
	// A tree whose WorkDir is unusable surfaces the sandbox failure.
	bad := filepath.Join(work, "occupied")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree2 := engine.NewTaskTree("sess_fsbad", "root", "root")
	tree2.WorkDir = bad
	if err := st.SaveTree(tree2); err != nil {
		t.Fatal(err)
	}
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&session=sess_fsbad")
	if status != http.StatusInternalServerError {
		t.Fatalf("bad workdir list: %d, want 500", status)
	}
	status, _ = fsGet(t, ts.URL+"/api/fs/file?token=test-token&session=sess_fsbad&path=x")
	if status != http.StatusInternalServerError {
		t.Fatalf("bad workdir file: %d, want 500", status)
	}

	// A live session resolves to its workdir (and redacts its secrets).
	secret := "sk-cov-secret-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(work, "leak.txt"), []byte("key="+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := createSimpleSession(t, ts, map[string]any{"workdir": work, "api_key": secret})
	waitLivePhase(t, ts, id, string(agent.PhasePlanReview))
	status, body = fsGet(t, ts.URL+"/api/fs/list?token=test-token&session="+id+"&path=.")
	if status != http.StatusOK || !strings.Contains(string(body), "leak.txt") {
		t.Fatalf("live session list: %d %s", status, body)
	}
	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&session="+id+"&path=leak.txt")
	if status != http.StatusOK {
		t.Fatalf("live session file: %d %s", status, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatal("the session secret must be masked in fs/file output")
	}
}

func TestCovFSListErrors(t *testing.T) {
	skipIfRoot := func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based failure injection does not work as root")
		}
	}
	skipIfRoot(t)
	work, data := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	_, ts := covAPIServer(t, work, data)

	// Escape via Resolve.
	status, _ := fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=../x")
	if status != http.StatusBadRequest {
		t.Fatalf("escape list: %d", status)
	}
	// Symlink pointing outside the workspace.
	if err := os.Symlink(outside, filepath.Join(work, "link-out")); err != nil {
		t.Fatal(err)
	}
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=link-out")
	if status != http.StatusForbidden {
		t.Fatalf("escaping symlink list: %d", status)
	}
	// A path under an escaping symlink: the parent check rejects it.
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=link-out/missing")
	if status != http.StatusForbidden {
		t.Fatalf("escape via parent symlink: %d", status)
	}
	// A confined but missing path is 404.
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=missing-dir")
	if status != http.StatusNotFound {
		t.Fatalf("missing dir list: %d", status)
	}
	// A file is not a directory.
	if err := os.WriteFile(filepath.Join(work, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=plain.txt")
	if status != http.StatusBadRequest {
		t.Fatalf("file as dir: %d", status)
	}
	// An unreadable directory cannot be listed.
	locked := filepath.Join(work, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	status, _ = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=locked")
	if status != http.StatusForbidden {
		t.Fatalf("unreadable dir list: %d", status)
	}
	// More than 2000 entries are truncated.
	if err := os.MkdirAll(filepath.Join(work, "many"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2001; i++ {
		if err := os.WriteFile(filepath.Join(work, "many", pad4(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status, body := fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=many")
	if status != http.StatusOK || !strings.Contains(string(body), `"truncated":true`) {
		t.Fatalf("truncated list: %d", status)
	}
}

func TestCovFSListEntryTypes(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	_, ts := covAPIServer(t, work, data)

	if err := os.WriteFile(filepath.Join(work, "in.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "zfile.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "in.txt"), filepath.Join(work, "link-in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(work, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(work, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, body := fsGet(t, ts.URL+"/api/fs/list?token=test-token")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	var list struct {
		Entries []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Escaped bool   `json:"escaped"`
		} `json:"entries"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	escaped := map[string]bool{}
	lastDir := -1
	firstNonDir := len(list.Entries)
	for i, e := range list.Entries {
		types[e.Name] = e.Type
		escaped[e.Name] = e.Escaped
		if e.Type == "dir" {
			lastDir = i
		} else if i < firstNonDir {
			firstNonDir = i
		}
	}
	if types["link-in"] != "symlink" || escaped["link-in"] {
		t.Fatalf("confined symlink = %+v", types)
	}
	if types["link-out"] != "symlink" || !escaped["link-out"] {
		t.Fatalf("escaping symlink = %+v", types)
	}
	if types["pipe"] != "other" {
		t.Fatalf("fifo type = %q", types["pipe"])
	}
	if types["in.txt"] != "file" || types["adir"] != "dir" || types["zfile.txt"] != "file" {
		t.Fatalf("basic types = %+v", types)
	}
	// Directories sort before everything else.
	if lastDir > firstNonDir && firstNonDir != len(list.Entries) {
		t.Fatalf("dirs must sort first (last dir %d, first non-dir %d)", lastDir, firstNonDir)
	}
}

func TestCovFSFileEdges(t *testing.T) {
	skipIfRoot := func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based failure injection does not work as root")
		}
	}
	skipIfRoot(t)
	work, data := t.TempDir(), t.TempDir()
	_, ts := covAPIServer(t, work, data)

	if err := os.WriteFile(filepath.Join(work, "in.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "in.txt"), filepath.Join(work, "link-in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "gone.txt"), filepath.Join(work, "link-dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "zdir"), filepath.Join(work, "link-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "zdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(work, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(work, "tenkb.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 10*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(work, "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	cases := []struct {
		path string
		want int
	}{
		{"missing.txt", http.StatusNotFound},
		{"link-in", http.StatusOK},
		{"link-dangling", http.StatusNotFound},
		{"link-dir", http.StatusBadRequest},
		{"pipe", http.StatusBadRequest},
		{"locked.txt", http.StatusInternalServerError},
		{"tenkb.txt", http.StatusOK},
	}
	for _, c := range cases {
		status, body := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path="+c.path)
		if status != c.want {
			t.Fatalf("file %q: %d (want %d) %s", c.path, status, c.want, body)
		}
	}
	// The symlinked file read goes through the target and returns its mode.
	status, body := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=link-in")
	if status != http.StatusOK || !strings.Contains(string(body), `"path":"link-in"`) {
		t.Fatalf("symlink file read: %d %s", status, body)
	}
	// A nested path exercises isWithin's prefix branch.
	status, _ = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=zdir/../in.txt")
	if status != http.StatusOK {
		t.Fatalf("nested clean path: %d", status)
	}
	// A session-less file read without a live session skips redaction.
	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=in.txt&session=nope")
	if status != http.StatusNotFound {
		t.Fatalf("file with unknown session: %d %s", status, body)
	}
}

// --- sse.go edge branches ----------------------------------------------------

func TestCovHandleEventsGuards(t *testing.T) {
	s, ts := newTestServer(t)
	_ = ts

	// Missing session id.
	rec := httptest.NewRecorder()
	s.handleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/sessions//events", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: %d", rec.Code)
	}

	// Existing tree file but a writer that cannot flush.
	if err := os.WriteFile(filepath.Join(s.dataDir, "sess1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/sess1/events", nil)
	req.SetPathValue("id", "sess1")
	s.handleEvents(&covNoFlushWriter{rec}, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("no flusher: %d", rec.Code)
	}
}

func TestCovHandleEventsBrokenClient(t *testing.T) {
	s, _ := newTestServer(t, WithToken("test-token"), WithHeartbeat(time.Millisecond))
	if err := os.WriteFile(filepath.Join(s.dataDir, "sess2.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/sess2/events", nil)
	req.SetPathValue("id", "sess2")
	s.handleEvents(&covFailWriter{w: rec, err: errors.New("client gone")}, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("headers must be written before the failure: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tree_snapshot") {
		t.Fatalf("snapshot must be sent first:\n%s", rec.Body.String())
	}
}

func TestCovReplayEventsEdges(t *testing.T) {
	s, _ := newTestServer(t)
	dir := t.TempDir()
	events := filepath.Join(dir, "e.jsonl")
	body := strings.Join([]string{
		"",
		"torn line without json",
		`{"seq":1,"kind":"a"}`,
		`{"seq":2,"kind":"b"}`,
		`{"seq":3,"kind":"c"}`,
		"",
	}, "\n")
	if err := os.WriteFile(events, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec0 := httptest.NewRecorder()
	flusher := http.Flusher(rec0)
	rec := httptest.NewRecorder()
	maxSeq := s.replayEvents(rec, flusher, events, 1, nil)
	// seq 1 is skipped by Last-Event-ID (raising the watermark), 2 and 3 replay.
	if maxSeq != 3 {
		t.Fatalf("maxSeq = %d", maxSeq)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"kind":"a"`) {
		t.Fatalf("replayed events below the resume point:\n%s", out)
	}
	if !strings.Contains(out, `"kind":"b"`) || !strings.Contains(out, `"kind":"c"`) {
		t.Fatalf("events above the resume point must replay:\n%s", out)
	}
	if !strings.Contains(out, `"replay":true`) || !strings.Contains(out, `"kind":"c"`) {
		t.Fatalf("event 3 must be tagged and replayed:\n%s", out)
	}

	// An unreadable event file ends the replay immediately.
	skipIfRoot(t)
	locked := filepath.Join(dir, "locked.jsonl")
	if err := os.WriteFile(locked, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	rec2 := httptest.NewRecorder()
	if got := s.replayEvents(rec2, flusher, locked, 7, nil); got != 7 {
		t.Fatalf("unreadable events must return afterSeq, got %d", got)
	}
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("chmod-based failure injection does not work as root")
	}
}

func TestCovCurrentTreeJSONAndTailLines(t *testing.T) {
	s, _ := newTestServer(t)
	// Missing tree file.
	if _, err := s.currentTreeJSON(nil, "nope", filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing tree must fail")
	}
	// tailLines: missing file is an empty tail.
	lines, err := tailLines(filepath.Join(t.TempDir(), "nope.jsonl"), 5)
	if err != nil || len(lines) != 0 {
		t.Fatalf("missing file tail = %v %v", lines, err)
	}
	skipIfRoot(t)
	locked := filepath.Join(t.TempDir(), "locked.jsonl")
	if err := os.WriteFile(locked, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := tailLines(locked, 5); err == nil {
		t.Fatal("unreadable event file must fail")
	}
	// Blank lines are skipped, and a line beyond the scanner ceiling errors.
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(ok, []byte("\n\n{\"a\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err = tailLines(ok, 5)
	if err != nil || len(lines) != 1 {
		t.Fatalf("blank-line tail = %v %v", lines, err)
	}
	big := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 17*1024*1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tailLines(big, 5); err == nil {
		t.Fatal("oversized line must fail the scanner")
	}
}

// --- helpers -----------------------------------------------------------------

func pad4(i int) string {
	d := strconv.Itoa(i)
	for len(d) < 4 {
		d = "0" + d
	}
	return d
}
