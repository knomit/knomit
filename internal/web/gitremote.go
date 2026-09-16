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

// splitGitRoute reads the repo name and the git-protocol suffix out of a
// mount-relative path, tolerating the two forms every git host accepts and
// users therefore type by habit: a ".git" suffix on the repo, and a trailing
// slash after it.
//
// They fail for DIFFERENT reasons without this, which is why both are handled
// explicitly rather than by one cleanup pass:
//
//	"kb.git/info/refs"  the repo segment reads as "kb.git", so the OUTER
//	                    lookup misses and the request 404s before any store
//	                    is touched.
//	"kb//info/refs"     the repo segment is right, but the remainder is
//	                    "/info/refs", which the caller rewrites to
//	                    "//info/refs" and the INNER mux does not match.
//
// Tolerance stops exactly there. No general suffix stripping, no slash
// collapsing, no case folding: each of those changes WHICH repository is
// named, and this endpoint is also how a subscribing instance identifies the
// knowledge base it follows. "kb.git.git" and "KB" are not this repo.
func splitGitRoute(p string) (repoName, suffix string) {
	repoName, suffix, _ = strings.Cut(p, "/")
	// At most one ".git", and never the whole segment: ".git" on its own stays
	// a repo NAMED ".git" rather than becoming a nameless one. (No repo can
	// actually be called that — isValidRepoName admits only [a-z0-9-_] — so
	// this is about not manufacturing an empty name, not about serving it.)
	if trimmed := strings.TrimSuffix(repoName, ".git"); trimmed != "" {
		repoName = trimmed
	}
	// One leading slash, from a URL written as ".../kb/". Anything more was
	// not typed by habit.
	suffix = strings.TrimPrefix(suffix, "/")
	return repoName, suffix
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
		repoName, repoSuffix := splitGitRoute(p)
		if repoName == "" {
			http.NotFound(w, r)
			return
		}

		ri := rm.Get(repoName)
		if ri == nil {
			http.NotFound(w, r)
			return
		}

		// Hold the acquisition for the whole git-protocol exchange (packfile
		// streaming can be long-lived); a concurrent Archive/SwapStore drains
		// this request before closing the store instead of closing it mid-fetch.
		svc, release, err := ri.Acquire()
		if err != nil {
			http.Error(w, "repo store unavailable", http.StatusServiceUnavailable)
			return
		}
		defer release()
		provider, ok := (interface{})(svc).(gitHTTPProvider)
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
