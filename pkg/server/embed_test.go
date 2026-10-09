package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/web"
)

// TestStatic_ServesEmbeddedDist is the TR-11.1 proof: the production serve
// binary injects web.DistFS(), so a stock Go checkout (no Node) must serve
// the committed, embedded UI rather than the placeholder. This runs in the
// Go-only CI jobs, unlike TestStatic_ServesBuiltDist which reads web/dist
// from disk.
func TestStatic_ServesEmbeddedDist(t *testing.T) {
	dist, ok := web.DistFS()
	if !ok {
		t.Fatal("embedded web/dist missing index.html; run `make web-build`")
	}

	s, err := New(agent.NewRunManager(), t.TempDir(),
		WithToken("test-token"),
		WithHeartbeat(50*time.Millisecond),
		WithWorkdir(t.TempDir()),
		WithStatic(dist),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `src="/assets/`) {
		t.Fatalf("root did not serve the embedded Vite shell (hashed asset tag missing):\n%s", body)
	}

	// Unknown client routes fall back to the embedded SPA shell.
	resp2, err := http.Get(ts.URL + "/sessions/sess_x")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(b2), `<div id="root">`) {
		t.Fatalf("SPA fallback: status=%d", resp2.StatusCode)
	}
}
