package web

import (
	"net/http"

	"knomit/internal/repos"
)

// SessionBindingMiddleware serves the unscoped /api/v1/mcp mount.
//
// It does exactly two things, and what it NO LONGER does is the point.
//
//  1. It marks the context session-scoped. That marker is what tells
//     knomit_bind it has something to bind, and what tells every other tool's
//     gate that a `binding` handle is required rather than forbidden.
//  2. It installs a repos.PinRecorder so the binding the tool gate resolves can
//     travel back out to recordClientSession, which runs after the response.
//
// It does NOT look up a binding. Routing used to be keyed on Mcp-Session-Id and
// resolved right here, which meant every request on one session id was served
// from whatever that session had bound LAST. A client may share a single
// connection — and so a single session id — across several logical jobs: Claude
// Desktop does exactly that for Cowork sessions, and on 2026-09-17 two jobs'
// knomit_bind calls overwrote each other seven times in half an hour, with four
// writes served from the wrong repo. Nothing on the wire identified the caller,
// and nothing could: MCP 2026-07-28 deleted protocol sessions outright and
// directs servers needing cross-call state to server-minted handles passed as
// ordinary tool arguments (SEP-2567).
//
// So the repo a call is served from is now named by that call's own `binding`
// argument, resolved inside the tool gate (internal/mcp), and NOTHING falls
// back to session state. The initialize body-peek that used to guard this
// lookup went with it: there is no longer any session-keyed state for a
// re-initialize to resolve from the wrong session.
func SessionBindingMiddleware(_ *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := repos.WithSessionScoped(r.Context())
			ctx = repos.WithPinRecorder(ctx, &repos.PinRecorder{})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
