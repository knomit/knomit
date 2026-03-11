//go:build noembed

package web

import "net/http"

// StaticHandler returns a 404 handler when compiled without embedded assets.
func StaticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

// newSPAHandler returns a 404 handler when compiled without embedded assets.
func newSPAHandler(_ http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}
}
