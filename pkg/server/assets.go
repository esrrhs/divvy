package server

import (
	"embed"
	"io/fs"
)

// placeholderFS holds the fallback homepage served when no built frontend
// (web/dist) is embedded. It keeps `go build ./...` green in a Go-only
// environment (NFR-2): the real assets are injected by the release/CI build.
//
//go:embed placeholder/index.html
var placeholderFS embed.FS

// defaultStaticFS serves the placeholder tree rooted at its own folder.
func defaultStaticFS() fs.FS {
	sub, err := fs.Sub(placeholderFS, "placeholder")
	if err != nil {
		// embed paths are compile-time constants; this cannot fail.
		panic(err)
	}
	return sub
}
