//go:build !noembed

// Package webui embeds the compiled React web UI assets.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// all:dist is required (not just `dist`): the build-generated assets are
// gitignored, so a fresh checkout's dist/ holds only the committed `.gitkeep`
// sentinel. Plain //go:embed silently drops dot-prefixed files, leaving the
// directory with "no embeddable files" and failing the build (CI builds Go
// directly, without `make web`). The `all:` prefix includes `.gitkeep`, so the
// package compiles everywhere; real UI assets come from `npm run build`.
//
//go:embed all:dist
var staticFiles embed.FS

// FS returns a sub-filesystem rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(staticFiles, "dist")
}

// Handler returns an http.Handler serving the embedded assets.
func Handler() http.Handler {
	sub, _ := FS()
	return http.FileServer(http.FS(sub))
}
