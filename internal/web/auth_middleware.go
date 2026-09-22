package web

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"knomit/internal/auth"
	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/web/hal"
)

// AuthMiddleware is the ONE place a transport becomes a Principal (F19
// revision 2, "Decision"). Order of evidence, strongest first:
//
//  1. Kernel peer credentials on a unix socket connection (auth.ConnContext
//     put them there) — a bridge principal keyed by uid, with the kernel's
//     pid carried alongside for the client_sessions row.
//  2. Nothing, from loopback, with [auth].require = false — the ANONYMOUS
//     principal, so an upgrade changes nobody's day. What it may do is the
//     parsed [auth].loopback_default, resolved in Require through
//     loopbackGrants rather than here: this function decides WHO, never WHAT.
//  3. Nothing, otherwise — no principal on the context at all. With
//     require = true the request ends here; without it, a downstream Require
//     still denies, because the zero principal holds nothing.
//
// The refusal is 403, never 401. RFC 7235 makes WWW-Authenticate mandatory on
// a 401, and phase 1 has no scheme a TCP caller could satisfy — the socket is
// the only credential, and it is not something a header can present. Phase 3
// introduces the 401 with a Bearer challenge on the routes a token unlocks.
// The two 403s are told apart by TITLE: "Authentication required" here (no
// principal at all) versus "Permission denied" in Require (a principal that
// lacks the permission). Clients and tests key on the title, not the status.
//
// Certificates (phase 2) and bearer tokens (phase 3) slot in between 1 and 2
// and produce the same Principal type, so nothing downstream changes.
func AuthMiddleware(cfg config.AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if uid, pid, ok := auth.PeerFromContext(ctx); ok {
				p := auth.Principal{Kind: auth.KindBridge, ID: "uid:" + strconv.Itoa(uid), Via: auth.ViaSocket}
				ctx = auth.WithPrincipal(ctx, p)
				ctx = sessions.WithVerifiedPID(ctx, pid)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if !cfg.Require && isLoopback(r.RemoteAddr) {
				ctx = auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindAnonymous, Via: auth.ViaNone})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if cfg.Require {
				hal.WriteProblem(w, http.StatusForbidden, "Authentication required",
					"this knomit instance requires a verified principal ([auth].require = true); connect over the unix socket",
					r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopback reports whether RemoteAddr is 127.0.0.0/8 or ::1. A unix
// connection has no ip and is handled before this is consulted.
//
// Do NOT widen this. It is the whole boundary between "a human at this
// machine, who had every right before [auth] existed" and the network, and
// widening it to fix a test would hand a LAN caller the anonymous
// principal's permissions. Tests that need the anonymous principal set
// RemoteAddr to 127.0.0.1 — httptest.NewRequest defaults to 192.0.2.1:1234,
// which is documentation space and deliberately not loopback.
func isLoopback(remoteAddr string) bool {
	ip := net.ParseIP(sessions.RemoteIP(remoteAddr))
	return ip != nil && ip.IsLoopback()
}

// loopbackGrants answers the anonymous principal from config and everyone
// else from the store. It is what Require and the MCP gate both consult, so
// the two enforcement points cannot come to disagree about anonymous.
type loopbackGrants struct {
	anon  auth.Set
	store auth.Grants
}

func (g loopbackGrants) For(ctx context.Context, p auth.Principal) (auth.Set, error) {
	if p.Kind == auth.KindAnonymous {
		return g.anon, nil
	}
	if g.store == nil {
		return nil, nil
	}
	return g.store.For(ctx, p)
}

// Require refuses with 403 unless the request's principal holds perm. The
// problem document names the permission so a client can ask for exactly it,
// and names the principal so an operator knows which grants row to write.
func Require(g auth.Grants, perm auth.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.FromContext(r.Context())
			if !auth.Allowed(r.Context(), g, p, perm) {
				hal.WriteProblem(w, http.StatusForbidden, "Permission denied",
					"this operation requires the "+string(perm)+" permission; principal "+p.String()+" does not hold it",
					r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
