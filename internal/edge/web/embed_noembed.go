//go:build noembed

package web

import "io/fs"

// embeddedStaticFS returns nil when compiled without embedded assets; the
// shared static handlers turn a nil FS into a 404.
func embeddedStaticFS() fs.FS { return nil }
