package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/web/hal"
)

// branchCtxKey is the private context key used to stash the decoded branch
// name after BranchMiddleware runs. Handlers read it via BranchFromContext.
type branchCtxKey struct{}

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
		name := hal.DecodeBranch(urlSeg)
		ctx := context.WithValue(r.Context(), branchCtxKey{}, name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// BranchFromContext retrieves the canonical branch name stashed by
// BranchMiddleware. Panics if the middleware did not run — this is a
// programming error, not a runtime condition.
func BranchFromContext(ctx context.Context) string {
	v, ok := ctx.Value(branchCtxKey{}).(string)
	if !ok {
		panic("BranchFromContext: no branch in context (BranchMiddleware must run first)")
	}
	return v
}
