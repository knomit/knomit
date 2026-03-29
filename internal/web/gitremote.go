package web

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"knomit/internal/repos"
)

// gitHTTPProvider is the narrow interface GitRemoteHandler needs — just the
// per-store HTTP handler for the git smart HTTP protocol.
type gitHTTPProvider interface {
	Handler() http.Handler
}

// GitRemoteHandler returns an http.Handler implementing the read-only Smart
// HTTP git protocol. It routes by repo name and delegates to git.Store.Handler().
//
//   - GET  /{repo}/info/refs?service=git-upload-pack — advertise refs
//   - POST /{repo}/git-upload-pack                   — serve a fetch
func GitRemoteHandler(rm *repos.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chi's RoutePath is the mount-relative path, e.g. "/knomit/info/refs".
		// Fall back to r.URL.Path when called outside a chi routing context (e.g. tests).
		routePath := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			routePath = rctx.RoutePath
		}
		p := strings.TrimPrefix(routePath, "/")
		repoName, repoSuffix, _ := strings.Cut(p, "/")
		if repoName == "" {
			http.NotFound(w, r)
			return
		}

		ri := rm.Get(repoName)
		if ri == nil {
			http.NotFound(w, r)
			return
		}

		var rawGS repos.GitStore
		ri.WithRead(func(d repos.StoreDeps) { rawGS = d.GS })
		provider, ok := rawGS.(gitHTTPProvider)
		if !ok {
			http.Error(w, "git serving not supported for this repo", http.StatusInternalServerError)
			return
		}

		// Rewrite the request URL to just the git-protocol suffix so the inner
		// mux can match /info/refs and /git-upload-pack directly.
		u2 := *r.URL
		u2.Path = "/" + repoSuffix
		u2.RawPath = ""
		r2 := r.WithContext(r.Context())
		r2.URL = &u2
		provider.Handler().ServeHTTP(w, r2)
	})
}
