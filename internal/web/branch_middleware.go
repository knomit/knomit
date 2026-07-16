package web

import (
	"context"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// BranchMiddleware extracts the {branch} URL parameter from the chi route
// context, decodes it (`:` → `/`), and stores the canonical branch name in
// the request context for downstream handlers. Routes that use {branch}
// must be registered under this middleware.
func BranchMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlSeg := chi.URLParam(r, "branch")
		if urlSeg == "" {
			hal.WriteProblem(w, http.StatusBadRequest,
				"Missing branch", "the {branch} URL segment is required", r.URL.Path)
			return
		}
		unescaped, err := url.PathUnescape(urlSeg)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest,
				"Invalid branch", "branch segment is not valid percent-encoded UTF-8", r.URL.Path)
			return
		}
		name := hal.DecodeBranch(unescaped)
		ctx := repos.WithBranch(r.Context(), name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// BranchFromContext retrieves the canonical branch name stashed by
// BranchMiddleware. Panics if the middleware did not run — this is a
// programming error, not a runtime condition.
func BranchFromContext(ctx context.Context) string {
	b, ok := repos.BranchFromContextOpt(ctx)
	if !ok {
		panic("BranchFromContext: no branch in context (BranchMiddleware must run first)")
	}
	return b
}
