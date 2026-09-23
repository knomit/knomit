package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"knomit/internal/auth"
	"knomit/internal/web/hal"
)

// OAuthHandler is the router of the OAuth listener ([oauth].addr, F19 phase
// 3a, R1) — the only listener a bearer token is judged on. It is built
// FRESH, not derived from Handler():
//
//   - the public OAuth surface (both discovery documents, /oauth/authorize,
//     /oauth/authorize/{id}/wait, /oauth/token, /oauth/revoke) with no
//     authentication at all, so [auth].require cannot hide it;
//   - the API and MCP tree under APIBase, behind BearerMiddleware and
//     Require(read) (R6) and NEVER AuthMiddleware (see apiRouter);
//   - nothing else. /git, /docs, the web UI and the approval endpoints are
//     not here; an unknown path is a 404 after authentication, so an
//     unauthenticated caller learns only that it needs a token.
//
// nil when [oauth] is not configured; the serve command then opens no
// listener for it.
func (s *Server) OAuthHandler() http.Handler {
	if s.OAuthIssuer == nil || s.BearerVerifier == nil {
		return nil
	}
	if s.mcpHandler == nil {
		s.buildMCPHandler()
	}
	// No loopbackGrants and no CertGrants: nobody on this listener is
	// anonymous or holds a certificate. The MCP handler, shared with the
	// plain listener, consults s.grants(), which reaches the same
	// TokenGrants for a token principal.
	g := auth.TokenGrants{Inner: s.Grants}
	bearer := BearerMiddleware(s.BearerVerifier, s.OAuthIssuer.Name())
	edge := func(next http.Handler) http.Handler { return bearer(Require(g, auth.Read)(next)) }

	public := s.OAuthIssuer.Routes()
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Handle("/.well-known/*", public)
	r.Handle("/oauth/*", public)
	r.Mount(APIBase, s.apiRouter(edge, g))
	r.NotFound(edge(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no resource at "+req.URL.Path, req.URL.Path)
	})).ServeHTTP)
	return r
}
