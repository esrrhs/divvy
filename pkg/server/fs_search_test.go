package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
)

type fsSearchResp struct {
	Query         string `json:"query"`
	Literal       bool   `json:"literal"`
	Regex         bool   `json:"regex"`
	CaseSensitive bool   `json:"case_sensitive"`
	Hits          []struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Preview string `json:"preview"`
	} `json:"hits"`
	Truncated bool `json:"truncated"`
	Scanned   int  `json:"scanned"`
}

func TestFS_SearchLiteralHitsAndIgnoreRules(t *testing.T) {
	work := t.TempDir()
	os.MkdirAll(filepath.Join(work, "pkg"), 0755)
	os.WriteFile(filepath.Join(work, "pkg", "add.go"), []byte("package pkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"), 0644)
	os.WriteFile(filepath.Join(work, "notes.txt"), []byte("func Add is mentioned here too\n"), 0644)
	os.MkdirAll(filepath.Join(work, ".git"), 0755)
	os.WriteFile(filepath.Join(work, ".git", "config"), []byte("func Add hidden in git\n"), 0644)
	os.MkdirAll(filepath.Join(work, "node_modules"), 0755)
	os.WriteFile(filepath.Join(work, "node_modules", "dep.js"), []byte("func Add hidden in modules\n"), 0644)
	os.WriteFile(filepath.Join(work, "blob.bin"), []byte{0x00, 0xff, 0xfe, 'A', 'd', 'd'}, 0644)

	_, ts := fsTestServer(t, work)
	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&q="+url.QueryEscape("func Add"))
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got fsSearchResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Literal || got.Regex {
		t.Fatalf("default should be literal: %+v", got)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("hits=%d body=%s", len(got.Hits), body)
	}
	paths := map[string]int{}
	for _, h := range got.Hits {
		if filepath.IsAbs(h.Path) || strings.Contains(h.Path, work) || strings.HasPrefix(h.Path, "/") {
			t.Fatalf("non-relative path leaked: %q", h.Path)
		}
		paths[h.Path] = h.Line
	}
	if paths["pkg/add.go"] != 3 || paths["notes.txt"] != 1 {
		t.Fatalf("unexpected hits: %+v", got.Hits)
	}
	if _, ok := paths[".git/config"]; ok {
		t.Fatal("ignored .git leaked")
	}
	if _, ok := paths["node_modules/dep.js"]; ok {
		t.Fatal("ignored node_modules leaked")
	}
}

func TestFS_SearchLiteralDoesNotTreatMetacharactersAsRegex(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("price is $5.00 today\nvalue is $50\n"), 0644)
	_, ts := fsTestServer(t, work)

	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&q="+url.QueryEscape("$5.00"))
	if status != http.StatusOK {
		t.Fatalf("literal status=%d body=%s", status, body)
	}
	var lit fsSearchResp
	if err := json.Unmarshal(body, &lit); err != nil {
		t.Fatal(err)
	}
	if len(lit.Hits) != 1 || lit.Hits[0].Line != 1 || !strings.Contains(lit.Hits[0].Preview, "$5.00") {
		t.Fatalf("literal hits=%+v", lit.Hits)
	}

	status, body = fsGet(t, ts.URL+"/api/fs/search?token=test-token&regex=1&q="+url.QueryEscape(`\$5\.\d+`))
	if status != http.StatusOK {
		t.Fatalf("regex status=%d body=%s", status, body)
	}
	var re fsSearchResp
	if err := json.Unmarshal(body, &re); err != nil {
		t.Fatal(err)
	}
	if !re.Regex || re.Literal || len(re.Hits) != 1 || re.Hits[0].Line != 1 {
		t.Fatalf("regex hits=%+v literal=%v", re.Hits, re.Literal)
	}

	status, body = fsGet(t, ts.URL+"/api/fs/search?token=test-token&regex=1&q=[unterminated")
	if status != http.StatusBadRequest {
		t.Fatalf("invalid regex status=%d body=%s", status, body)
	}
}

func TestFS_SearchCaseSensitivityAndEmptyQuery(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("Hello World\n"), 0644)
	_, ts := fsTestServer(t, work)

	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&q=hello")
	if status != http.StatusOK {
		t.Fatalf("status=%d %s", status, body)
	}
	var ins fsSearchResp
	json.Unmarshal(body, &ins)
	if len(ins.Hits) != 1 {
		t.Fatalf("default case-insensitive: %+v", ins)
	}

	status, body = fsGet(t, ts.URL+"/api/fs/search?token=test-token&case=1&q=hello")
	if status != http.StatusOK {
		t.Fatalf("status=%d %s", status, body)
	}
	var cs fsSearchResp
	json.Unmarshal(body, &cs)
	if len(cs.Hits) != 0 {
		t.Fatalf("case-sensitive should miss: %+v", cs)
	}

	status, _ = fsGet(t, ts.URL+"/api/fs/search?token=test-token&q=")
	if status != http.StatusBadRequest {
		t.Fatalf("empty q status=%d", status)
	}
}

func TestFS_SearchScopedToConversationAndRedactsSecrets(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "creds.txt"), []byte("token=sk-search-secret-value\n"), 0644)
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "creds.txt"), []byte("token=sk-search-secret-value in other project\n"), 0644)

	s, ts := fsTestServer(t, work)
	c, err := newConversation(work, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "search")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(s.dataDir, c, nil); err != nil {
		t.Fatal(err)
	}
	s.setAPIKey(c.ID, "sk-search-secret-value")

	otherC, err := newConversation(other, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(s.dataDir, otherC, nil); err != nil {
		t.Fatal(err)
	}

	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&conversation="+c.ID+"&q=sk-search-secret-value")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got fsSearchResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "creds.txt" {
		t.Fatalf("hits=%+v", got.Hits)
	}
	if strings.Contains(got.Hits[0].Preview, "sk-search-secret-value") {
		t.Fatalf("secret leaked in preview: %q", got.Hits[0].Preview)
	}
	if !strings.Contains(got.Hits[0].Preview, "***REDACTED***") {
		t.Fatalf("expected redaction marker, got %q", got.Hits[0].Preview)
	}

	// Other conversation must not see this workdir's files.
	status, body = fsGet(t, ts.URL+"/api/fs/search?token=test-token&conversation="+otherC.ID+"&q=other+project")
	if status != http.StatusOK {
		t.Fatalf("other status=%d body=%s", status, body)
	}
	json.Unmarshal(body, &got)
	if len(got.Hits) != 1 || got.Hits[0].Path != "creds.txt" {
		t.Fatalf("other project hits=%+v", got.Hits)
	}

	status, _ = fsGet(t, ts.URL+"/api/fs/search?token=test-token&conversation=conv_missing&q=x")
	if status != http.StatusNotFound {
		t.Fatalf("missing conversation status=%d", status)
	}
}

func TestFS_SearchDoesNotReturnSymlinkEscape(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside-secret-token\n"), 0644)
	os.WriteFile(filepath.Join(work, "ok.txt"), []byte("inside-only\n"), 0644)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(work, "link.txt")); err != nil {
		t.Fatal(err)
	}
	_, ts := fsTestServer(t, work)
	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&q=outside-secret-token")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got fsSearchResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 0 {
		t.Fatalf("expected no hits, got %+v", got.Hits)
	}
	if strings.Contains(string(body), "secret.txt") || strings.Contains(string(body), outside) {
		t.Fatalf("outside path leaked: %s", body)
	}
}

func TestFS_SearchSkipsNULBinary(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "mixed.bin"), []byte("func Add\x00embedded"), 0644)
	os.WriteFile(filepath.Join(work, "ok.txt"), []byte("func Add is text\n"), 0644)
	_, ts := fsTestServer(t, work)
	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&q="+url.QueryEscape("func Add"))
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got fsSearchResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "ok.txt" {
		t.Fatalf("NUL binary leaked into search: %+v", got.Hits)
	}
}

func TestFS_SearchRedactsBeforeClipAndQueryEcho(t *testing.T) {
	work := t.TempDir()
	secret := "sk-search-secret-value"
	// Secret starts just before the 200-byte clip point so a clip-then-redact
	// path would leave a unique key prefix in the preview.
	line := strings.Repeat("x", 185) + secret + " trailing marker\n"
	os.WriteFile(filepath.Join(work, "cfg.log"), []byte(line), 0644)

	s, ts := fsTestServer(t, work)
	c, err := newConversation(work, ConversationSettings{AgentMode: agent.AgentLoopDirect}, "search")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConversation(s.dataDir, c, nil); err != nil {
		t.Fatal(err)
	}
	s.setAPIKey(c.ID, secret)

	status, body := fsGet(t, ts.URL+"/api/fs/search?token=test-token&conversation="+c.ID+"&q="+url.QueryEscape("trailing marker")+"&glob="+url.QueryEscape("*.log"))
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got fsSearchResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "cfg.log" {
		t.Fatalf("hits=%+v", got.Hits)
	}
	if strings.Contains(got.Hits[0].Preview, secret) || strings.Contains(got.Hits[0].Preview, "sk-search-sec") {
		t.Fatalf("secret or unique prefix leaked in clipped preview: %q", got.Hits[0].Preview)
	}
	if !strings.Contains(got.Hits[0].Preview, "***REDACTED***") {
		t.Fatalf("expected redaction marker in clipped preview, got %q", got.Hits[0].Preview)
	}
	if !strings.HasSuffix(got.Hits[0].Preview, "...") {
		t.Fatalf("expected display clip, got %q", got.Hits[0].Preview)
	}

	// Searching for the secret itself must not echo it back in query.
	status, body = fsGet(t, ts.URL+"/api/fs/search?token=test-token&conversation="+c.ID+"&q="+url.QueryEscape(secret))
	if status != http.StatusOK {
		t.Fatalf("secret-q status=%d body=%s", status, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Query, secret) || strings.Contains(string(body), secret) {
		t.Fatalf("secret leaked in search response: %s", body)
	}
	if got.Query != "***REDACTED***" && !strings.Contains(got.Query, "***REDACTED***") {
		t.Fatalf("expected redacted query echo, got %q", got.Query)
	}
}
