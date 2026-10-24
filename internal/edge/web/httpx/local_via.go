package httpx

import (
	"net/http"

	"knomit/internal/edge/web/hal"
	"knomit/internal/foundation/auth"
)

// RequireLocalVia admits only a principal the local authenticated listener
// vouched for — a unix-socket uid, or a named-pipe SID on Windows
// (auth.LocalVia) — and refuses everyone else with 403, whatever they hold.
// The OAuth approval endpoints use it instead of Require(admin) (R5): the
// anonymous loopback principal holds admin by default, and a browser can
// reach loopback TCP but not the socket, so "admin" would let any page the
// operator visits approve a request.
func RequireLocalVia(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalVia(r) {
			p, _ := auth.FromContext(r.Context())
			hal.WriteProblem(w, http.StatusForbidden, "Permission denied",
				"this operation is only available over the local authenticated listener (the unix socket, or the named pipe on Windows); principal "+p.String()+" did not arrive over it",
				r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsLocalVia is RequireLocalVia's test: the request's principal came in over
// the local authenticated listener (auth.LocalVia), as the edge vouched for
// it. Nothing the client sends (a header, a query) can make it true. GET
// /fleet reports it as `local`, so a page knows whether the mutations
// RequireLocalVia guards are reachable from where it runs.
func IsLocalVia(r *http.Request) bool {
	p, _ := auth.FromContext(r.Context())
	return p.Via == auth.LocalVia
}
