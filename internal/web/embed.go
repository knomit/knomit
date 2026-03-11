//go:build !noembed

package web

import (
	"net/http"

	webui "knomit/web"
)

// StaticHandler returns an HTTP handler serving the embedded web assets.
func StaticHandler() http.Handler {
	return webui.Handler()
}

// newSPAHandler wraps the static file handler with SPA fallback to index.html.
func newSPAHandler(static http.Handler) http.HandlerFunc {
	distFS, _ := webui.FS()
	return func(w http.ResponseWriter, r *http.Request) {
		if distFS == nil {
			static.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		if len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}
		if path == "" {
			path = "index.html"
		}
		f, err := distFS.Open(path)
		if err != nil {
			// Not found — serve index.html for client-side routing.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/index.html"
			static.ServeHTTP(w, r2)
			return
		}
		f.Close()
		static.ServeHTTP(w, r)
	}
}

