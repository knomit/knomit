package synthesize

import "context"

// The actor is the correlation handle recorded on a session's row as
// pipeline_sessions.created_by (knomit#123). It travels on the context because
// it is request-scoped and read EXACTLY ONCE — at StartSession, next to the
// branch binding — after which it lives on the row like Branch and Scoped. The
// alternative, threading it through NewReviewer → NewReviewerWithEffort →
// NewReviewerWithOptions → NewPipeline and NewHypothesizer, would ripple five
// constructors and every test call site to deliver a value nothing below
// StartSession ever reads again.
//
// Context is already this codebase's carrier for request-scoped identity
// (repos.BindingFromContext, obs/reqinfo.FromContext); this is the same shape.
// It stops at the engine boundary on purpose: the store takes createdBy as an
// explicit parameter, so nothing below Pipeline reaches into a context for it.
//
// What the value MEANS — that it is client-supplied and unverified, and must
// never be read as authentication — is documented at the two ends that own it:
// the schema column comment and internal/mcp/actor.go, which composes it.

type actorKey struct{}

// WithActor returns a context carrying the correlation handle for whoever is
// making this request. Set at the MCP request boundary; read by
// Pipeline.StartSession.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// actorFromContext returns the handle WithActor stored, or "" when there is
// none. Empty is a legitimate, reachable answer — every in-process caller
// (tests, local tools driving Reviewer directly) has no request to attribute
// to — so this deliberately has no "unknown" sentinel and no error.
func actorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorKey{}).(string)
	return actor
}
