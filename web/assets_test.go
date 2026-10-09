package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDistFS_EmbeddedBuild proves the committed/built UI is embedded into
// the Go binary (TR-11.1): a Go-only checkout must expose a real Vite
// shell and its hashed assets without Node at build time.
func TestDistFS_EmbeddedBuild(t *testing.T) {
	dist, ok := DistFS()
	if !ok {
		t.Fatal("DistFS unavailable: web/dist/index.html missing; run `make web-build`")
	}

	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	html := string(index)
	if !strings.Contains(html, `<div id="root">`) {
		t.Fatalf("embedded index.html is not the Vite app shell:\n%s", html)
	}

	// Every asset the shell references must exist in the embedded tree.
	found := false
	remain := html
	for _, ref := range []string{`src="/assets/`, `href="/assets/`} {
		for {
			i := strings.Index(remain, ref)
			if i < 0 {
				break
			}
			found = true
			tail := remain[i+len(ref):] // starts at the file name
			end := strings.IndexByte(tail, '"')
			if end < 0 {
				preview := tail
				if len(preview) > 60 {
					preview = preview[:60]
				}
				t.Fatalf("unterminated asset reference: %q", preview)
			}
			asset := "assets/" + tail[:end]
			if _, err := fs.Stat(dist, asset); err != nil {
				t.Fatalf("embedded shell references missing asset %s: %v", asset, err)
			}
			remain = tail[end:]
		}
	}
	if !found {
		t.Fatal("embedded index.html references no /assets/ files")
	}
}
