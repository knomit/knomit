package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/edge/oauth"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki/pkitest"
)

// reachEcho answers which of alpha and beta the request's recorded reach
// covers ("ab", "a", "b" or ""), and 204 when no principal was minted.
func reachEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := ""
		if auth.ReachesRepo(r.Context(), "alpha") {
			out += "a"
		}
		if auth.ReachesRepo(r.Context(), "beta") {
			out += "b"
		}
		if _, ok := auth.FromContext(r.Context()); !ok {
			w.WriteHeader(http.StatusNoContent)
		}
		_, _ = w.Write([]byte(out))
	})
}

// auth.ReachesRepo is DEFAULT-DENY, so every edge that mints a principal must
// record a reach. The local edges (auth disabled, socket or pipe peer, TLS
// certificate, loopback) record every repo: grants carry no repo dimension.
// The bearer edge records the token's audience. A request that gets NO
// principal gets no reach either.
func TestEveryAuthEdgeRecordsARepoReach(t *testing.T) {
	inst := pkitest.New(t).Enroll(t, "peer@agents.knomit.io")
	serve := func(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	local := AuthMiddleware(config.AuthConfig{Require: false}, false)(reachEcho())

	for _, tc := range []struct {
		name string
		h    http.Handler
		req  *http.Request
	}{
		{"auth disabled", AuthMiddleware(config.AuthConfig{}, true)(reachEcho()), httptest.NewRequest("GET", "/x", nil)},
		{"socket or pipe peer", local, httptest.NewRequest("GET", "/x", nil).WithContext(auth.WithPeer(context.Background(), testPeer(501, 7)))},
		{"TLS certificate", local, tlsRequest("10.0.0.7:5555", inst.Cert)},
		{"loopback", local, fromLoopback(httptest.NewRequest("GET", "/x", nil))},
	} {
		if rr := serve(tc.h, tc.req); rr.Code != http.StatusOK || rr.Body.String() != "ab" {
			t.Fatalf("%s: code=%d reach=%q, want every repo", tc.name, rr.Code, rr.Body.String())
		}
	}

	remote := httptest.NewRequest("GET", "/x", nil)
	remote.RemoteAddr = "10.0.0.9:4444"
	if rr := serve(local, remote); rr.Code != http.StatusNoContent || rr.Body.String() != "" {
		t.Fatalf("no principal, so no reach: code=%d reach=%q", rr.Code, rr.Body.String())
	}

	f := newOAuthWebFixture(t, testIssuer)
	bearer := BearerMiddleware(oauth.NewVerifier(testIssuer, f.store), testIssuer)(reachEcho())
	for _, tc := range []struct{ resource, path, want string }{
		{testIssuer + alphaMCP, alphaMCP, "a"}, // one repo's endpoint: that repo
		{testIssuer + "/api/v1/repos/beta", "/api/v1/repos/beta/branches/agent:test/facts", "b"},
		{testIssuer + "/api/v1/repos/alphabet", "/api/v1/repos/alphabet/x", ""}, // a "/" boundary, not a prefix
		{testIssuer + "/api/v1/lenses/eng/mcp", "/api/v1/lenses/eng/mcp", ""},   // a lens: its mounts, not repos by name
		{testIssuer + "/api/v1/mcp", "/api/v1/mcp", "ab"},                       // unscoped: binds anything
		{testIssuer + "/api/v1", alphaMCP, "ab"},
	} {
		tok := f.mint(t, "laptop", tc.resource, []string{"read"}, auth.Read)
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if rr := serve(bearer, req); rr.Code != http.StatusOK || rr.Body.String() != tc.want {
			t.Fatalf("token for %s: code=%d reach=%q, want %q", tc.resource, rr.Code, rr.Body.String(), tc.want)
		}
	}
}

// The MCP HTTPContextFunc copies the reach as it copies the principal and
// ceiling: a ctx built from scratch would otherwise refuse every cross-KB read.
func TestMCPHTTPContext_CopiesTheRepoReach(t *testing.T) {
	r := httptest.NewRequest("POST", alphaMCP, nil)
	r = r.WithContext(auth.WithRepoReach(r.Context(), func(repo string) bool { return repo == "alpha" }))
	ctx := mcpHTTPContext(context.Background(), r)
	if !auth.ReachesRepo(ctx, "alpha") || auth.ReachesRepo(ctx, "beta") {
		t.Fatal("the request's reach must be copied, unchanged")
	}
	if auth.ReachesRepo(mcpHTTPContext(context.Background(), httptest.NewRequest("POST", alphaMCP, nil)), "alpha") {
		t.Fatal("no reach on the request, so none on the ctx (default deny)")
	}
}
