// Package web embeds the built desktop UI assets (web/dist).
//
// The Vite build output is committed to the repository, so a Go-only
// checkout can build and serve the real UI without Node installed
// (TR-11.1). Regenerate it with `make web-build` (or `npm run build` in
// web/); the web CI job fails if the committed tree drifts from a fresh
// build (TR-11.2).
package web

import (
	"embed"
	"io/fs"
)

// all:dist also embeds files Vite may emit with a leading . or _.
//
//go:embed all:dist
var distEmbed embed.FS

// DistFS returns the frontend asset tree rooted at the Vite output
// directory. The boolean is false when no real build is embedded (no
// index.html), in which case callers leave the server on its built-in
// placeholder page.
func DistFS() (fs.FS, bool) {
	sub, err := fs.Sub(distEmbed, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}
