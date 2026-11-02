package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// manageParamValue is a plausible value for a declared parameter of a
// management GET route: the request must get past the parameter check,
// whatever the handler then makes of the value.
var manageParamValue = map[string]string{"limit": "5"}

// Every management API GET route (user ruling 2026-11-02, extending #718 and
// #720 past the knowledge-base routes) answers 400 "Invalid parameter" to a
// query parameter it does not declare (`unknown parameter "<name>"`) and to a
// malformed query string (`malformed query string: <parse error>`), and lets
// every parameter its spec route declares past the check. The requests come
// over the local listener as the operator, so /fleet/root's RequireLocalVia
// admits them and the check is what answers. A route marked ok answers its
// bare URL 200, so the check is reached on a live route, not a 404.
//
// The route list is checked against the router: every GET whose handler
// lives in package manage must be in it, so a new GET route cannot skip the
// refusal unnoticed.
//
// Sabotage, one group at a time: drop a route's RefuseUnknown call (its row
// answers 200 or another status to ?bogus=1); swap url.ParseQuery for
// r.URL.Query() in httpx.RefuseUnknown (the malformed rows pass); drop
// "limit" from fleetActivityParams (its declared request is the
// unknown-parameter 400); delete a row (the router-coverage check fails).
func TestManageRoutes_UnknownAndMalformedAre400_DeclaredAccepted(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	_, err := e.m.Create(context.Background(), repos.CreateSpec{Name: "gamma", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	archived, err := e.m.Archive("gamma")
	require.NoError(t, err)
	alpha := e.m.Get("alpha")
	r := (&Server{Manager: e.m, Grants: localOperatorGrants(), AgentBranch: alpha.AgentBranch(), OntologyRoot: "kb"}).NewAPIRouter()
	get := func(u string) (int, string) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, localPeer(httptest.NewRequest(http.MethodGet, u, nil)))
		return rec.Code, rec.Body.String()
	}

	routes := []struct {
		route, spec, url string
		ok               bool // the bare URL answers 200
	}{
		{"GET /repo-events", "/repo-events", "/repo-events", false}, // a stream: only refusals are read
		{"GET /fleet", "/fleet", "/fleet", true},
		{"GET /fleet/members", "/fleet/members", "/fleet/members", false},   // not in a fleet
		{"GET /fleet/activity", "/fleet/activity", "/fleet/activity", false}, // not in a fleet
		{"GET /fleet/root", "/fleet/root", "/fleet/root", false},            // no root held
		{"GET /archived", "/archived", "/archived", true},
		{"GET /archived/{id}", "/archived/{id}", "/archived/" + archived.ID, true},
		{"GET /repos", "/repos", "/repos", true},
		{"GET /repo-creates", "/repo-creates", "/repo-creates", true},
		{"GET /repo-creates/{id}", "/repo-creates/{id}", "/repo-creates/nope", false}, // no such job: 404 after the check
		{"GET /ontologies/presets", "/ontologies/presets", "/ontologies/presets", true},
		{"GET /ontologies/presets/{name}", "/ontologies/presets/{name}", "/ontologies/presets/default", true},
		{"GET /ontologies/schema", "/ontologies/schema", "/ontologies/schema", true},
		{"GET /templates", "/templates", "/templates", true},
		{"GET /repos/{repo}/", "/repos/{repo}", "/repos/alpha", true},
		{"GET /repos/{repo}/templates", "/repos/{repo}/templates", "/repos/alpha/templates", true},
		{"GET /repos/{repo}/origin", "/repos/{repo}/origin", "/repos/alpha/origin", false}, // no origin: 404 after the check
		{"GET /repos/{repo}/branches", "/repos/{repo}/branches", "/repos/alpha/branches", true},
		{"GET /repos/{repo}/pushed-branches", "/repos/{repo}/pushed-branches", "/repos/alpha/pushed-branches", true},
		{"GET /repos/{repo}/experiments", "/repos/{repo}/experiments", "/repos/alpha/experiments", true},
		{"GET /repos/{repo}/branches/{branch}/", "/repos/{repo}/branches/{branch}", "/repos/alpha/branches/" + urlBranch(alpha.AgentBranch()), true},
		{"GET /lenses", "/lenses", "/lenses", true},
		{"GET /lenses/{lens}/", "/lenses/{lens}", "/lenses/eng", true},
	}

	// Router coverage: every GET served by package manage is a row.
	var all []routeChain
	walkFull(r, "", nil, &all)
	listed := map[string]bool{}
	for _, rt := range routes {
		listed[rt.route] = true
	}
	served := 0
	for _, rc := range all {
		if strings.HasPrefix(rc.route, "GET ") && strings.Contains(rc.endpoint, "/api/manage.") {
			served++
			require.True(t, listed[rc.route], "manage GET route %s (%s) has no row", rc.route, rc.endpoint)
		}
	}
	require.Equal(t, len(routes), served, "every row is a manage GET route the router serves")

	problem := func(body string) (string, string) {
		var p struct{ Title, Detail string }
		_ = json.Unmarshal([]byte(body), &p)
		return p.Title, p.Detail
	}
	refusedForParam := func(code int, body string) bool {
		_, d := problem(body)
		return code == http.StatusBadRequest && (strings.HasPrefix(d, "unknown parameter") || strings.HasPrefix(d, "malformed query string"))
	}
	for _, rt := range routes {
		if rt.ok {
			code, body := get(rt.url)
			require.Equal(t, http.StatusOK, code, "%s bare: %s", rt.url, body)
		}
		code, body := get(rt.url + "?bogus=1")
		require.Equal(t, http.StatusBadRequest, code, "%s ?bogus: %s", rt.url, body)
		title, detail := problem(body)
		require.Equal(t, "Invalid parameter", title, body)
		require.Equal(t, `unknown parameter "bogus"`, detail, body)
		for raw, want := range map[string]string{
			"%zz=1":     `malformed query string: invalid URL escape "%zz"`,
			"bogus;x=1": "malformed query string: invalid semicolon separator in query",
		} {
			code, body := get(rt.url + "?" + raw)
			require.Equal(t, http.StatusBadRequest, code, "%s ?%s: %s", rt.url, raw, body)
			title, detail := problem(body)
			require.Equal(t, "Invalid parameter", title, body)
			require.Equal(t, want, detail, body)
		}
		for _, n := range specQueryParams(t, rt.spec) {
			v, ok := manageParamValue[n]
			require.True(t, ok, "no test value for %s on %s", n, rt.spec)
			code, body := get(rt.url + "?" + url.Values{n: {v}}.Encode())
			require.False(t, refusedForParam(code, body), "%s ?%s=%s refused as a parameter: %s", rt.url, n, v, body)
		}
	}
}
