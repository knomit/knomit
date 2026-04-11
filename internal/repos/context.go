package repos

import "context"

type contextKey string

const repoInstanceKey contextKey = "repoInstance"

// WithRepoInstance stores a RepoInstance in the context.
func WithRepoInstance(ctx context.Context, ri *RepoInstance) context.Context {
	return context.WithValue(ctx, repoInstanceKey, ri)
}

// RepoFromContext retrieves the RepoInstance from the request context.
// Panics if not present (middleware must always set it).
func RepoFromContext(ctx context.Context) *RepoInstance {
	ri, ok := ctx.Value(repoInstanceKey).(*RepoInstance)
	if !ok {
		panic("RepoFromContext: no RepoInstance in context")
	}
	return ri
}

// RepoFromContextOpt retrieves the RepoInstance from the context if present.
// Returns (nil, false) if the context has no RepoInstance. Use this from code
// paths where the repo may legitimately be absent (e.g. MCP initialize hooks
// called outside a request-scoped middleware chain). Most code should use
// RepoFromContext instead, which panics to enforce the middleware contract.
func RepoFromContextOpt(ctx context.Context) (*RepoInstance, bool) {
	ri, ok := ctx.Value(repoInstanceKey).(*RepoInstance)
	return ri, ok
}
