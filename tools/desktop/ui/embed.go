// Package desktopui embeds the desktop-only UI (Settings and Logs). It is
// SEPARATE from knomit/web on purpose: web/ is embedded in the server binary
// too, and a form that changes this app's port or tails its log file has no
// meaning in server mode.
//
// Deliberately NOT behind the `desktop` build tag, matching its sibling
// packages under tools/desktop/internal/: this package is pure Go with no Wails
// dependency, and tagging it would make `go vet ./...` and a plain `go build
// ./...` skip it entirely — the embed directive below would then only ever be
// checked in tagged builds, which is exactly where a missing dist/.gitkeep is
// most expensive to discover.
package desktopui

import (
	"embed"
	"io/fs"
)

// all:dist for the same reason as web/embed.go — dist/ holds only a committed
// .gitkeep on a fresh checkout, and plain //go:embed silently drops dot-files,
// leaving "no embeddable files" and failing the build for anyone who has not
// run `make desktop-ui`.
//
//go:embed all:dist
var staticFiles embed.FS

// FS returns a sub-filesystem rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(staticFiles, "dist")
}
