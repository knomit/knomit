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

// branchCtxKey is the private context key for the branch a request is bound
// to. web.BranchMiddleware stores the decoded {branch} URL segment here so
// packages that cannot import internal/web (e.g. internal/mcp) can read it.
type branchCtxKey struct{}

// WithBranch stores the bound branch name in the context.
func WithBranch(ctx context.Context, branch string) context.Context {
	return context.WithValue(ctx, branchCtxKey{}, branch)
}

// BranchFromContextOpt retrieves the bound branch name if present.
func BranchFromContextOpt(ctx context.Context) (string, bool) {
	b, ok := ctx.Value(branchCtxKey{}).(string)
	return b, ok
}

// sessionScopedKey marks a request as served by the UNSCOPED MCP mount
// (/api/v1/mcp), where the repo or lens comes from the session's stored
// binding rather than from the URL. Only web.SessionBindingMiddleware sets it.
// knomit_bind refuses to run without it: on a URL-scoped mount there is
// nothing for it to change.
type sessionScopedKey struct{}

// WithSessionScoped marks ctx as belonging to the session-bound mount.
func WithSessionScoped(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionScopedKey{}, true)
}

// SessionScoped reports whether ctx came from the session-bound mount.
func SessionScoped(ctx context.Context) bool {
	v, _ := ctx.Value(sessionScopedKey{}).(bool)
	return v
}

// bindingErrCtxKey carries WHY the session's stored pin could not be resolved.
//
// The middleware cannot render this as an HTTP failure: the caller speaks
// JSON-RPC, and a 4xx would break the framing and tell the agent nothing it
// could act on. So the reason rides in the context and the tool handler turns
// it into a tool error the agent can actually read.
type bindingErrCtxKey struct{}

// WithBindingError stores the reason the stored binding did not resolve.
func WithBindingError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, bindingErrCtxKey{}, err)
}

// BindingErrorFromContext retrieves that reason if one was stored.
func BindingErrorFromContext(ctx context.Context) (error, bool) {
	err, ok := ctx.Value(bindingErrCtxKey{}).(error)
	return err, ok
}
