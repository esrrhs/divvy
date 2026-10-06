package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

func fsTestServer(t *testing.T, workdir string) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(agent.NewRunManager(), t.TempDir(),
		WithToken("test-token"), WithHeartbeat(50*time.Millisecond), WithWorkdir(workdir))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func fsGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TR-5.3: normal listing respects the ignore list and reads text files.
func TestFS_ListAndRead(t *testing.T) {
	work := t.TempDir()
	os.MkdirAll(filepath.Join(work, "sub"), 0755)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("alpha\n"), 0644)
	os.WriteFile(filepath.Join(work, "sub", "b.txt"), []byte("beta\n"), 0644)
	os.MkdirAll(filepath.Join(work, ".git"), 0755)
	os.WriteFile(filepath.Join(work, ".git", "config"), []byte("secretish"), 0644)
	os.MkdirAll(filepath.Join(work, "node_modules"), 0755)

	_, ts := fsTestServer(t, work)
	status, body := fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=.")
	if status != http.StatusOK {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	var list struct {
		Entries []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range list.Entries {
		names[e.Name] = true
	}
	if !names["a.txt"] || !names["sub"] {
		t.Fatalf("expected a.txt and sub, got %v", names)
	}
	if names[".git"] || names["node_modules"] {
		t.Fatalf("ignored directories must be hidden, got %v", names)
	}

	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=sub/b.txt")
	if status != http.StatusOK {
		t.Fatalf("file status=%d body=%s", status, body)
	}
	var file struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	if file.Content != "beta\n" {
		t.Fatalf("content=%q", file.Content)
	}
}

// TR-5.3: traversal attempts and absolute paths outside the workspace fail.
func TestFS_PathTraversalRejected(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "ok.txt"), []byte("ok"), 0644)
	_, ts := fsTestServer(t, work)

	for _, p := range []string{
		"../../../../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"/etc/passwd",
		"/etc",
		"sub/../../../../etc/passwd",
	} {
		status, body := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path="+p)
		if status == http.StatusOK {
			t.Fatalf("path %q must not resolve (status=%d body=%s)", p, status, body)
		}
		if status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusNotFound {
			t.Fatalf("path %q: status=%d, want 4xx", p, status)
		}
	}

	// Empty path is a bad request too.
	status, _ := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=")
	if status != http.StatusBadRequest {
		t.Fatalf("empty path status=%d", status)
	}
}

// TR-5.3: symlinks pointing outside the workspace are 403 on read and
// flagged escaped in listings; an in-workspace symlink works.
func TestFS_SymlinkEscapeRejected(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "inside.txt"), []byte("inside"), 0644)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("outside"), 0644)

	if err := os.Symlink(outside, filepath.Join(work, "evil.lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.txt", filepath.Join(work, "good.lnk")); err != nil {
		t.Fatal(err)
	}

	_, ts := fsTestServer(t, work)

	// Escaping symlink read → 403.
	status, body := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=evil.lnk")
	if status != http.StatusForbidden {
		t.Fatalf("escaping symlink status=%d body=%s", status, body)
	}
	if !strings.Contains(string(body), "escapes workspace") {
		t.Fatalf("403 body should explain escape: %s", body)
	}

	// Listing flags it but stays 200.
	status, body = fsGet(t, ts.URL+"/api/fs/list?token=test-token&path=.")
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	var list struct {
		Entries []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Escaped bool   `json:"escaped"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list.Entries {
		if e.Name == "evil.lnk" {
			found = true
			if e.Type != "symlink" || !e.Escaped {
				t.Fatalf("evil.lnk should be an escaped symlink: %+v", e)
			}
		}
		if e.Name == "good.lnk" && e.Escaped {
			t.Fatal("inside symlink must not be flagged escaped")
		}
	}
	if !found {
		t.Fatal("evil.lnk missing from listing")
	}

	// In-workspace symlink reads fine and returns target content.
	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=good.lnk")
	if status != http.StatusOK {
		t.Fatalf("inside symlink status=%d body=%s", status, body)
	}
	if !strings.Contains(string(body), "inside") {
		t.Fatalf("symlink content wrong: %s", body)
	}
}

// TR-5.4: oversized and binary files return explicit errors, never a crash.
func TestFS_BinaryAndOversizedRejected(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "blob.bin"), []byte{'P', 'K', 0x03, 0x04, 0x00}, 0644)
	big := make([]byte, fsMaxFileBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	os.WriteFile(filepath.Join(work, "big.txt"), big, 0644)

	_, ts := fsTestServer(t, work)

	status, body := fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=blob.bin")
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("binary status=%d body=%s", status, body)
	}

	status, body = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=big.txt")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d body=%s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["max_size"] == nil {
		t.Fatalf("oversize error must state the limit: %s", body)
	}

	// Reading a directory as a file is a clear 400.
	status, _ = fsGet(t, ts.URL+"/api/fs/file?token=test-token&path=.")
	if status != http.StatusBadRequest {
		t.Fatalf("directory as file status=%d", status)
	}
}
