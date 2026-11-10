package web

// The unified authenticator (plan P3, Task 3.4). BUILT, NOT MOUNTED: nothing
// in Handler(), NewAPIRouter or OAuthHandler wraps a request in it until the
// flip (P4). Until then the old AuthMiddleware decides everything, and a
// session cookie or PAT sent to the main port is ignored.
//
// It turns a request into exactly one principal, materializes that
// principal's policy and puts it in the context (authz.WithPolicy). Order,
// first match wins; once a credential is PRESENTED, a later one is never
// consulted, and a presented credential that fails is a 401 — never a
// fallback to anonymous:
//
//  1. a local peer (unix socket / named pipe, kernel credentials) ⇒ its OS
//     principal, via socket|pipe;
//  2. a TLS client certificate ⇒ an INSTANCE certificate only ⇒
//     node:<fingerprint> with the implicit node role, via mtls;
//  3. Authorization: Bearer ⇒ a PAT (knomit_pat_ prefix) or an OAuth access
//     token, via bearer;
//  4. Authorization: Basic ⇒ the username is ignored, the password is a token
//     as in 3, via basic (git clients);
//  5. the knomit_session cookie ⇒ the session's principal, via session; an
//     unsafe method must carry the Origin of the external address the
//     request matched, else 403 "Cross-origin request refused";
//  6. nothing ⇒ anonymous, via none.
//
// Nothing here reads the peer ADDRESS or decides by Host. The Host only picks
// which CONFIGURED external address a request arrived on (config.MatchIssuer),
// which sets the cookie's Secure flag and the Origin a cookie mutation must
// carry. A token is valid whatever Host it arrives on (§15.2).

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/edge/oauth"
	"knomit/internal/edge/web/hal"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
)

// sessionCookie is the browser session cookie's name.
const sessionCookie = "knomit_session"

// authenticator holds what authentication reads. The credential store is the
// concrete *oauth.Store (sessions, PATs and OAuth access tokens alike): the
// web package declares no interfaces (test/archtest web_no_interfaces).
type authenticator struct {
	mat     *authz.Materializer
	store   *oauth.Store
	issuers []*url.URL // config.Issuers(); MatchIssuer per request
}

// identified is what one credential resolved to.
type identified struct {
	principal authz.Principal
	via       authz.Via
	implicit  []authz.Role
	repoReach func(string) bool
	lensReach func(string) bool
	pid       int // a local peer's PID, for client sessions
}

func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Test builds attach a policy through the P2 seam, consulted first.
		if injectedPolicy != nil {
			if pol, ok := injectedPolicy(r); ok {
				ctx := auth.WithLensReach(auth.WithRepoReach(r.Context(), auth.AllRepos), auth.AllLenses)
				next.ServeHTTP(w, r.WithContext(authz.WithPolicy(ctx, pol)))
				return
			}
		}
		id, ok := a.identify(w, r)
		if !ok {
			return // identify answered
		}
		pol, err := a.mat.For(r.Context(), id.principal, id.via, id.implicit...)
		if err != nil {
			log.Error().Err(err).Str("principal", id.principal.String()).Msg("auth: materialize policy")
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable",
				"the caller's permissions could not be read; try again", r.URL.Path)
			return
		}
		ctx := authz.WithPolicy(r.Context(), pol)
		ctx = auth.WithRepoReach(ctx, id.repoReach)
		ctx = auth.WithLensReach(ctx, id.lensReach)
		if id.pid != 0 {
			ctx = clientsessions.WithVerifiedPID(ctx, id.pid)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identify runs the order above. On false it has written the response.
func (a *authenticator) identify(w http.ResponseWriter, r *http.Request) (identified, bool) {
	all := identified{repoReach: auth.AllRepos, lensReach: auth.AllLenses}

	// 1. Local peer.
	if peer, ok := auth.PeerFromContext(r.Context()); ok {
		id := all
		id.principal, id.via, id.pid = authz.FromPeer(peer), authz.ViaSocket, peer.PID
		if peer.Via == auth.ViaPipe {
			id.via = authz.ViaPipe
		}
		return id, true
	}

	// 2. TLS client certificate. The listener's tls.Config verifies the
	// chain to the fleet root and the CRL at the handshake (pki.ServerConfig);
	// what is checked here is the kind: an instance certificate, nothing else.
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cert, err := pki.IdentityOf(r.TLS.PeerCertificates[0])
		if err != nil || !cert.IsInstance() {
			a.refuse(w, r, "a TLS client certificate authenticates enrolled instances only; this one is not an instance certificate")
			return identified{}, false
		}
		id := all
		id.principal = authz.Principal{Kind: authz.KindNode, ID: cert.Fingerprint}
		id.via, id.implicit = authz.ViaMTLS, []authz.Role{authz.RoleNode}
		return id, true
	}

	// 3, 4. Authorization header.
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		if len(values) != 1 {
			a.refuse(w, r, "send one Authorization header")
			return identified{}, false
		}
		scheme, cred, _ := strings.Cut(values[0], " ")
		cred = strings.TrimSpace(cred)
		var via authz.Via
		switch {
		case strings.EqualFold(scheme, "Bearer"):
			via = authz.ViaBearer
		case strings.EqualFold(scheme, "Basic"):
			via = authz.ViaBasic
			raw, err := base64.StdEncoding.DecodeString(cred)
			if err != nil {
				a.refuse(w, r, "the Basic credential is not base64")
				return identified{}, false
			}
			// The username is ignored (x-token, git, anything or nothing):
			// the password is the token.
			_, pw, ok := strings.Cut(string(raw), ":")
			if !ok {
				a.refuse(w, r, "the Basic credential has no password")
				return identified{}, false
			}
			cred = pw
		default:
			a.refuse(w, r, "unsupported Authorization scheme; use Bearer (or Basic with a token as the password)")
			return identified{}, false
		}
		id, err := a.token(r.Context(), cred)
		if err != nil {
			if !errors.Is(err, oauth.ErrInvalid) {
				log.Error().Err(err).Msg("auth: verify token")
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "the token could not be checked; try again", r.URL.Path)
				return identified{}, false
			}
			a.refuse(w, r, "the token is invalid, expired or revoked")
			return identified{}, false
		}
		id.via = via
		return id, true
	}

	// 5. Session cookie.
	if cookies := sessionCookies(r); len(cookies) > 0 {
		if len(cookies) != 1 {
			a.refuseSession(w, r, "more than one session cookie")
			return identified{}, false
		}
		sess, err := a.store.LookupSession(r.Context(), cookies[0])
		if err != nil {
			if !errors.Is(err, oauth.ErrInvalid) {
				log.Error().Err(err).Msg("auth: look up session")
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "the session could not be checked; try again", r.URL.Path)
				return identified{}, false
			}
			a.refuseSession(w, r, "the session has ended; sign in again")
			return identified{}, false
		}
		if !safeMethod(r.Method) && !a.sameOrigin(r) {
			hal.WriteProblem(w, http.StatusForbidden, "Cross-origin request refused",
				"a signed-in browser may change data only from a page served by one of this instance's external addresses (the Origin must equal the address this request came in on)",
				r.URL.Path)
			return identified{}, false
		}
		id := all
		id.principal, id.via = sess.Principal, authz.ViaSession
		return id, true
	}

	// 6. Nothing presented.
	id := all
	id.principal, id.via = authz.Anonymous(), authz.ViaNone
	return id, true
}

// token resolves a bearer or Basic-password token: a PAT by its prefix,
// anything else as an OAuth access token. Every refusal is oauth.ErrInvalid.
func (a *authenticator) token(ctx context.Context, tok string) (identified, error) {
	if tok == "" {
		return identified{}, oauth.ErrInvalid
	}
	if strings.HasPrefix(tok, oauth.PATPrefix) {
		pat, err := a.store.VerifyPAT(ctx, tok)
		if err != nil {
			return identified{}, err
		}
		return identified{principal: pat.Principal(), repoReach: auth.AllRepos, lensReach: auth.AllLenses}, nil
	}
	p, audience, err := a.store.VerifyBearer(ctx, tok)
	if err != nil {
		return identified{}, err
	}
	return identified{principal: p, repoReach: tokenRepoReach(audience), lensReach: tokenLensReach(audience)}, nil
}

// sessionCookies are the values of every knomit_session cookie on r.
func sessionCookies(r *http.Request) []string {
	var out []string
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			out = append(out, c.Value)
		}
	}
	return out
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin: the request's Origin equals the origin of the external address
// its Host matched. No matched address, or no Origin, is not same-origin.
func (a *authenticator) sameOrigin(r *http.Request) bool {
	iss, ok := config.MatchIssuer(a.issuers, r)
	if !ok {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin != "" && len(r.Header.Values("Origin")) == 1 && strings.EqualFold(origin, iss.Scheme+"://"+iss.Host)
}

// isGitPath: the git transport, which gets a Basic challenge.
func isGitPath(p string) bool { return p == "/git" || strings.HasPrefix(p, "/git/") }

// refuse answers 401 with the challenge the path's clients understand:
// Basic for git, Bearer (with resource_metadata when the Host matched a
// listed external address) everywhere else.
func (a *authenticator) refuse(w http.ResponseWriter, r *http.Request, why string) {
	log.Info().Str("reason", why).Str("path", r.URL.Path).Str("remote", r.RemoteAddr).Msg("auth: credential refused")
	if isGitPath(r.URL.Path) {
		w.Header().Set("WWW-Authenticate", `Basic realm="knomit"`)
	} else {
		challenge := `Bearer realm="knomit"`
		if iss, ok := config.MatchIssuer(a.issuers, r); ok {
			if p, ok := canonicalRequestPath(r); ok {
				challenge += `, resource_metadata="` + oauth.ResourceMetadataURL(iss.String(), p) + `"`
			}
		}
		w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token"`)
	}
	hal.WriteProblem(w, http.StatusUnauthorized, "Invalid credential", why, r.URL.Path)
}

// refuseSession is refuse plus a Set-Cookie that deletes the dead cookie,
// so the browser stops presenting it and shows the login page.
func (a *authenticator) refuseSession(w http.ResponseWriter, r *http.Request, why string) {
	iss, _ := config.MatchIssuer(a.issuers, r)
	http.SetCookie(w, newSessionCookie(iss, "", -1))
	a.refuse(w, r, why)
}

// newSessionCookie is the one shape of the session cookie: HttpOnly,
// SameSite=Strict, Path=/, and Secure iff the external address the request
// matched is https (a nil issuer — no match — is Secure: fail closed).
// maxAge < 0 deletes it.
func newSessionCookie(iss *url.URL, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   iss == nil || iss.Scheme == "https",
		SameSite: http.SameSiteStrictMode,
	}
}
