package repos

import (
	"context"
	"sync"
)

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

// PinRecorder is a MUTABLE box the unscoped MCP mount puts in the request
// context so the resolved binding can travel BACKWARDS, from the tool handler
// out to the HTTP layer.
//
// It exists because handle resolution moved. On a URL-scoped mount the binding
// is in the context before the handler runs, so recordClientSession can read it
// off the request. On the unscoped mount the binding is named by an argument
// inside the JSON-RPC body, so nothing knows it until the tool gate runs — and
// a handler cannot alter the context its caller holds. The gate writes the pin
// here instead, and recordClientSession — which runs AFTER the response, and so
// after the handler — reads it out.
//
// What it records is still OBSERVATIONAL: client_sessions.binding is a trail of
// what a session was seen doing, and must never gate anything. Routing is
// decided by the handle argument alone.
//
// A task-augmented tool call keeps running after the response is written, so
// the write and the read genuinely race; the mutex is not decoration.
type PinRecorder struct {
	mu  sync.Mutex
	pin string
}

// Record stores the pin the gate resolved. Last write wins: one request carries
// one tool call, and a second would be a later, equally true observation.
func (p *PinRecorder) Record(pin string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pin = pin
}

// Pin returns what was recorded, or "" when nothing was.
func (p *PinRecorder) Pin() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pin
}

// pinRecorderKey carries the box. Only web.SessionBindingMiddleware installs
// one; every other mount resolves its binding before the handler and needs no
// return path.
type pinRecorderKey struct{}

// WithPinRecorder installs a recorder for this request.
func WithPinRecorder(ctx context.Context, p *PinRecorder) context.Context {
	return context.WithValue(ctx, pinRecorderKey{}, p)
}

// PinRecorderFromContext retrieves the request's recorder if one was installed.
func PinRecorderFromContext(ctx context.Context) (*PinRecorder, bool) {
	p, ok := ctx.Value(pinRecorderKey{}).(*PinRecorder)
	return p, ok
}
