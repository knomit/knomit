//go:build !noembed

// Package webui embeds the compiled React web UI assets.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
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
