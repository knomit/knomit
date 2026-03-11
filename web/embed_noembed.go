//go:build noembed

// Package webui provides stubs when compiled without embedded assets.
package webui

import (
	"io/fs"
	"net/http"
)

// FS returns nil when built without embedded assets.
func FS() (fs.FS, error) {
	return nil, nil
}

// Handler returns a 404 handler when built without embedded assets.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}
