package web

import (
	"context"
	"net"
	"net/http"

	"knomit/internal/auth"
	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/web/hal"
)

// AuthMiddleware is the ONE place a transport becomes a Principal (F19
// revision 2, "Decision"). Order of evidence, strongest first:
//
//  1. Kernel peer credentials on a LOCAL connection — a unix socket, or a
//     Windows named pipe (auth.ConnContext put them there) — as a bridge
//     principal keyed by uid or SID, with the OS-reported pid carried
//     alongside for the client_sessions row. Which of the two a platform
//     uses is auth.LocalVia; this function never asks.
//  2. Nothing, from loopback, with [auth].require = false — the ANONYMOUS
//     principal, so an upgrade changes nobody's day. What it may do is the
//     parsed [auth].loopback_default, resolved in Require through
//     loopbackGrants rather than here: this function decides WHO, never WHAT.
//  3. Nothing, otherwise — no principal on the context at all. With
//     require = true the request ends here; without it, a downstream Require
//     still denies, because the zero principal holds nothing.
//
// The refusal is 403, never 401. RFC 7235 makes WWW-Authenticate mandatory on
// a 401, and no listener this middleware serves has a scheme a TCP caller
// could satisfy — the local listener is the only credential, and it is not
// something a header can present. The ONE 401 in knomit is on the OAuth
// listener ([oauth].addr, F19 phase 3a), whose router never runs this
// middleware: BearerMiddleware is its only edge. An Authorization header on
// any listener served here is ignored. The two 403s are told apart by TITLE: "Authentication required" here (no
// principal at all) versus "Permission denied" in Require (a principal that
// lacks the permission). Clients and tests key on the title, not the status.
//
// 1b (phase 2) sits between 1 and 2: a request on the TLS listener becomes
// the instance (or operator) principal of its verified client certificate,
// and is refused outright if it has none. Bearer tokens (phase 3) do NOT
// slot in here: they are judged only on the OAuth listener, by
// BearerMiddleware, and produce the same Principal type there.
func AuthMiddleware(cfg config.AuthConfig, disabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if disabled {
				next.ServeHTTP(w, r.WithContext(
					auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindAnonymous, Via: auth.ViaNone})))
				return
			}
			if peer, ok := auth.PeerFromContext(ctx); ok {
				// Peer.Principal, never a literal built here: app.seedOwnPrincipal
				// seeds the grant with auth.LocalPrincipal, and the two have to
				// render the same string or the seeded row matches no request
				// (knomit#245, defect B). One formatting function per platform,
				// fed from both ends.
				ctx = auth.WithPrincipal(ctx, peer.Principal())
				ctx = sessions.WithVerifiedPID(ctx, peer.PID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if auth.IsTLSListener(ctx) {
				// 1b. The TLS listener (F19 phase 2) is NEVER anonymous, reads
				// included, whatever cfg.Require says: it exists only for
				// enrolled instances. Its tls.Config (pki.ServerConfig) uses
				// RequireAnyClientCert and verifies EVERYTHING in
				// VerifyConnection, which leaves VerifiedChains empty — so the
				// principal is read from PeerCertificates[0], and that is safe
				// only because no connection reaches here without the verifier
				// having accepted it. The refusals below are the belt to that
				// brace; nothing re-checks the chain or the CRL here.
				if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
					hal.WriteProblem(w, http.StatusForbidden, "Authentication required",
						"the TLS listener accepts only enrolled instances; no client certificate on this request", r.URL.Path)
					return
				}
				id, err := pki.IdentityOf(r.TLS.PeerCertificates[0])
				if err != nil {
					hal.WriteProblem(w, http.StatusForbidden, "Authentication required",
						"the TLS listener accepts only enrolled instances; the client certificate names no knomit identity", r.URL.Path)
					return
				}
				p := auth.InstancePrincipal(id.Fingerprint)
				if id.Role == pki.RoleOperator {
					p = auth.OperatorPrincipal(id.Fingerprint)
				}
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(ctx, p)))
				return
			}
			if !cfg.Require && isLoopback(r.RemoteAddr) {
				ctx = auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindAnonymous, Via: auth.ViaNone})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if cfg.Require {
				hal.WriteProblem(w, http.StatusForbidden, "Authentication required",
					"this knomit instance requires a verified principal ([auth].require = true); connect over the local authenticated listener (the unix socket, or the named pipe on Windows)",
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
