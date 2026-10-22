//go:build !noembed

package web

import (
	"io/fs"

	webui "knomit/web"
)

// embeddedStaticFS returns the compiled web UI embedded in the binary, or nil
// if it could not be opened. It is the only build-tagged part of static
// serving; everything that wraps it lives in static.go and is shared.
func embeddedStaticFS() fs.FS {
	sub, err := webui.FS()
	if err != nil {
		return nil
	}
	return sub
}
