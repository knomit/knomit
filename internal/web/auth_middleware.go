package web

import (
	"context"
	"net/http"
	"strconv"

	"knomit/internal/auth"
	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/platform/hostguard"
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
//  2. A request on the TLS listener (F19 phase 2) — the instance (or
//     operator) principal of its verified client certificate. That listener
//     is never anonymous, whatever [auth].require says: a request with no
//     certificate, or one naming no knomit identity, is refused here.
//  3. Nothing, from loopback, with [auth].require = false — the ANONYMOUS
//     principal, so an upgrade changes nobody's day — provided the Host
//     header names this machine (localhost, an IP literal, the bind host or
//     an [auth].loopback_hosts name; loopbackHostOK). Any other Host is a
//     DNS-rebound page and gets 421 (#281). What it may do is the
//     parsed [auth].loopback_default, resolved in Require through
//     loopbackGrants rather than here: this function decides WHO, never WHAT.
//  4. Nothing, otherwise — no principal on the context at all. With
//     require = true the request ends here; without it, a downstream Require
//     still denies, because the zero principal holds nothing.
//
// An authentication refusal here is 403, never 401 (the 421 in 3 refuses a
// Host, not a caller). RFC 7235 makes WWW-Authenticate mandatory on a 401,
// and no listener this middleware serves has an HTTP authentication scheme: the local listener's credential is the connection
// itself, and the TLS listener's is the client certificate verified in the
// handshake — neither is something a header can present. The ONE 401 in
// knomit is on the OAuth listener ([oauth].addr, F19 phase 3a), whose router
// never runs this middleware: BearerMiddleware is its only edge, and bearer
// tokens are judged there and nowhere else. An Authorization header on any
// listener served here is ignored.
//
// The two 403s are told apart by TITLE: "Authentication required" here (no
// principal at all) versus "Permission denied" in Require (a principal that
// lacks the permission). Clients and tests key on the title, not the status.
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
				// 2. The TLS listener (F19 phase 2) is NEVER anonymous, reads
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
				// #281: the anonymous principal is for a person at this
				// machine. A DNS-rebound web page is ALSO a loopback peer —
				// its browser connects to 127.0.0.1 — but its Host names the
				// attacker's domain, and it would otherwise read and write
				// everything loopback_default allows. Refused here, before a
				// principal exists, rather than in a separate middleware:
				// this branch runs exactly when anonymous is about to be
				// minted, after the socket and TLS listeners have returned
				// above, and the OAuth listener never runs this middleware.
				if !loopbackHostOK(r.Host, cfg.LoopbackHosts) {
					hal.WriteProblem(w, http.StatusMisdirectedRequest, "Misdirected Request",
						"this knomit instance does not answer loopback requests for Host "+strconv.Quote(r.Host)+
							"; a web page served from that name may not act as the local user. If this is your own proxy or tunnel, add the name to [auth].loopback_hosts in knomit.toml — anyone who reaches knomit through it then holds [auth].loopback_default",
						r.URL.Path)
					return
				}
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
	return hostguard.LoopbackPeer(remoteAddr)
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
