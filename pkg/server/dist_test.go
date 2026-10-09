package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

// TR-7.1: when web/dist is present (built by the web job or committed in a
// release), the Go server serves the real app shell and its hashed assets
// instead of the placeholder. Skipped in the Go-only CI job where Node is
// unavailable.
func TestStatic_ServesBuiltDist(t *testing.T) {
	distPath := filepath.Join("..", "..", "web", "dist")
	if _, err := os.Stat(filepath.Join(distPath, "index.html")); err != nil {
		t.Skip("web/dist not built (run `npm run build` in web/); Go-only CI keeps the placeholder")
	}

	s, err := New(agent.NewRunManager(), t.TempDir(),
		WithToken("test-token"),
		WithHeartbeat(50*time.Millisecond),
		WithWorkdir(t.TempDir()),
		WithStatic(osDirFS(distPath)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// Root serves the Vite-built shell (has the root div + module script),
	// not the built-in placeholder text.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `src="/assets/`) &&
		!strings.Contains(string(body), `src='/assets/`) {
		t.Fatalf("built index missing hashed asset tag:\n%s", body)
	}

	// Every referenced hashed asset resolves with 200.
	for _, ref := range []string{`src="/assets/`, `href="/assets/`} {
		i := strings.Index(string(body), ref)
		if i < 0 {
			continue
		}
		start := i + len(ref) - len("/assets/")
		q := string(body)[start:]
		end := strings.IndexAny(q, `"`)
		if end < 0 {
			t.Fatalf("unterminated asset ref: %q", q[:60])
		}
		assetPath := q[:end]
		aresp, err := http.Get(ts.URL + "/" + assetPath)
		if err != nil {
			t.Fatal(err)
		}
		ab, _ := io.ReadAll(aresp.Body)
		aresp.Body.Close()
		if aresp.StatusCode != http.StatusOK || len(ab) == 0 {
			t.Fatalf("asset %s: status=%d size=%d", assetPath, aresp.StatusCode, len(ab))
		}
	}

	// SPA client route falls back to the shell.
	resp2, err := http.Get(ts.URL + "/sessions/whatever")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(b2), `<div id="root">`) {
		t.Fatalf("SPA fallback wrong: status=%d", resp2.StatusCode)
	}
}

func osDirFS(path string) fs.FS {
	return os.DirFS(path)
}
