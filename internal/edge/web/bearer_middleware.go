package web

import (
	"net/http"
	"path"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/edge/oauth"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/auth"
)

// BearerMiddleware is the OAuth listener's ONLY way to a principal (R1): a
// verified bearer token becomes Principal{host, <subject>, token} plus the
// ceiling auth.TokenGrants intersects with. There is no anonymous, socket or
// certificate principal on that listener, and this is the only place in
// knomit that answers 401. It does so in exactly two cases, both carrying
// WWW-Authenticate: Bearer with resource_metadata:
//
//   - no Authorization header (RFC 6750 §3.1: no error code), and
//   - an Authorization header of ANY scheme or shape that does not verify:
//     unknown, expired, revoked, or not for this resource (error="invalid_token").
//
// A token that verifies but lacks a permission is a 403 downstream, as
// everywhere else. The request path must be canonical first: the audience is
// judged on it, and a path the router would read differently ("..", "//", an
// encoded "/") is a 400 before any token is looked at.
func BearerMiddleware(v *oauth.Verifier, issuer string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := canonicalRequestPath(r)
			if !ok {
				hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
					"the request path is not canonical (no '..', '//' or encoded '/' on this listener)", r.URL.Path)
				return
			}
			challenge := `Bearer realm="knomit", resource_metadata="` + oauth.ResourceMetadataURL(issuer, p) + `"`
			values := r.Header.Values("Authorization")
			if len(values) == 0 {
				w.Header().Set("WWW-Authenticate", challenge)
				hal.WriteProblem(w, http.StatusUnauthorized, "Authentication required",
					"this listener accepts only OAuth bearer tokens; the resource_metadata in WWW-Authenticate says how to get one", r.URL.Path)
				return
			}
			invalid := func(reason string) {
				log.Info().Str("reason", reason).Str("path", p).Str("remote", r.RemoteAddr).Msg("oauth: bearer refused")
				w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token", error_description="the access token is invalid, expired, revoked or not for this resource"`)
				hal.WriteProblem(w, http.StatusUnauthorized, "Invalid token",
					"the access token is invalid, expired, revoked or not for this resource", r.URL.Path)
			}
			tok, ok := bearerToken(values)
			if !ok {
				invalid("malformed Authorization header")
				return
			}
			principal, ceiling, audience, err := v.Verify(r.Context(), tok, p)
			if err != nil {
				invalid(err.Error())
				return
			}
			ctx := auth.WithPrincipal(r.Context(), principal)
			ctx = auth.WithCeiling(ctx, ceiling)
			ctx = auth.WithRepoReach(ctx, tokenRepoReach(audience))
			ctx = auth.WithLensReach(ctx, tokenLensReach(audience))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// tokenRepoReach is which repos a token with this audience (a path on the
// OAuth listener, "" for the whole issuer) can read:
//   - a token whose audience covers a repo's routes (/, /api/v1, /api/v1/repos)
//     reaches that repo;
//   - a token for one repo's endpoint (/api/v1/repos/<name>/...) reaches that
//     repo and no other;
//   - a token for the UNSCOPED MCP endpoint (/api/v1/mcp, or any path under
//     it: httpx.IsUnscopedMCP, the set the router serves unscoped) reaches
//     every repo:
//     its client binds a repo or lens after the token is issued, and that
//     endpoint may bind any of them (decisions/mcp/session-binding/
//     scoped-endpoint-bind 3408dd94; the scoped endpoints are the confined
//     ones);
//   - any other audience (a lens's endpoint, a non-repo route) reaches no
//     repo by name; a lens binding's own mounts are readable through the
//     binding, not through this.
func tokenRepoReach(audience string) func(repo string) bool {
	if httpx.IsUnscopedMCP(audience) {
		return auth.AllRepos
	}
	return func(repo string) bool {
		root := httpx.APIBase + "/repos/" + repo
		return pathUnderOrEqual(root, audience) || pathUnderOrEqual(audience, root)
	}
}

// tokenLensReach is tokenRepoReach for lenses, by the same rule: a token whose
// audience covers a lens's routes (/, /api/v1, /api/v1/lenses) or sits under
// one lens's (/api/v1/lenses/<name>/...) reaches that lens; the unscoped MCP
// endpoint's token reaches every lens (its client may bind any); a token for
// one repo reaches no lens.
func tokenLensReach(audience string) func(lens string) bool {
	if httpx.IsUnscopedMCP(audience) {
		return auth.AllLenses
	}
	return func(lens string) bool {
		root := httpx.APIBase + "/lenses/" + lens
		return pathUnderOrEqual(root, audience) || pathUnderOrEqual(audience, root)
	}
}

// pathUnderOrEqual: p is prefix itself or extends it at a "/" boundary; ""
// (the whole issuer) covers everything.
func pathUnderOrEqual(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}

// bearerToken accepts exactly one Authorization header of the form
// "Bearer <token>", scheme case-insensitive (RFC 7235 §2.1). Two headers are
// refused rather than one picked: which one a proxy added is not knowable.
// The token's characters are not checked here — anything that is not a
// token this issuer minted fails the lookup, which is the check.
func bearerToken(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

// canonicalRequestPath returns the decoded path when it is in its one
// spelling — path.Clean-stable apart from one trailing "/", and with no
// encoded "/" (chi routes on RawPath when it is set, so "%2F" would be one
// segment to the router and two to the audience check).
func canonicalRequestPath(r *http.Request) (string, bool) {
	p := r.URL.Path
	if strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		return "", false
	}
	trimmed := p
	if len(trimmed) > 1 {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	if !strings.HasPrefix(p, "/") || path.Clean(trimmed) != trimmed {
		return "", false
	}
	return p, true
}
