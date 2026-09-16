package web

import (
	"errors"
	"net/http"

	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
)

// SessionBindingMiddleware serves the unscoped /api/v1/mcp mount. It marks the
// context session-scoped and, when the request carries an Mcp-Session-Id with
// a stored binding, resolves it exactly as LensMiddleware does for a URL. A
// stored pin that no longer resolves is placed in the context as an error —
// never rendered as an HTTP failure — so the JSON-RPC framing survives and the
// tool handler is the one that tells the agent to bind again. The initialize
// request has no session id yet and passes through unbound by design.
func SessionBindingMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := repos.WithSessionScoped(r.Context())
			sid := r.Header.Get("Mcp-Session-Id")
			store := m.ClientSessions()
			if sid != "" && store != nil {
				pin, ok, err := store.SessionBinding(ctx, sid)
				switch {
				case err != nil:
					log.Warn().Err(err).Str("mcp_session", sid).Msg("session binding: lookup failed")
					ctx = repos.WithBindingError(ctx,
						errors.New("session binding lookup failed — retry, or call knomit_bind again"))
				case ok:
					if rctx, rerr := repos.ResolveSessionBinding(ctx, m, pin); rerr != nil {
						ctx = repos.WithBindingError(ctx, rerr)
					} else {
						ctx = rctx
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
