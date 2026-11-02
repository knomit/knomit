package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// kbParamValue is a plausible value for a declared parameter of the other KB
// read routes: the request must get past the parameter check, whatever the
// handler then makes of the value.
var kbParamValue = map[string]string{
	"path": "kb", "q": "x", "sort": "name", "limit": "5", "offset": "0", "prefix": "", "category": "domain",
	"axis": "recent", "since": "x", "cursor": "x", "after": "x", "from": "x", "before": "x",
	"fallback": "before", "log": "1", "run": "run-00000000000000000000000000000000", "repo": "beta",
	"depth": "1", "max_nodes": "10", "unpin": "true", "cross_kb": "false",
}

// Every other knowledge-base GET route (user ruling 2026-11-02, extending
// #718) answers 400 "Invalid parameter" to a query parameter it does not
// declare (`unknown parameter "<name>"`) and to a malformed query string
// (`malformed query string: <parse error>`), and lets every parameter its
// spec route declares past the check. A route with a body to read answers
// its bare URL 200, so the check is reached on a live route, not a 404.
//
// Sabotage, one group at a time: drop a route's refuseUnknown call (its row
// answers 200/other to the unknown parameter); drop a name from a route's set
// (that name's request is the unknown-parameter 400); route the fact read's
// /commits sub-resource to factReadParams (its `limit` is refused).
func TestKBRoutes_UnknownAndMalformedAre400_DeclaredAccepted(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	alpha := e.m.Get("alpha")
	writeFactsInOneCommit(t, alpha, factSpec{path: "kb/a/1.md", title: "alpha one", mod: func(f *fact.Fact) {
		f.Domain = []string{"d"}
		f.Motifs = []string{"config-drift"}
	}})
	var sha string
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		var err error
		sha, err = svc.HeadCommit(context.Background(), alpha.AgentBranch())
		require.NoError(t, err)
	}))
	h := e.as(t, "reader")
	br := "/api/v1/repos/alpha/branches/" + urlBranch(alpha.AgentBranch())
	lens := "/api/v1/lenses/eng"

	routes := []struct {
		spec, url string
		ok        bool // the bare URL answers 200
	}{
		{"/repos/{repo}/branches/{branch}/topics", br + "/topics", true},
		{"/repos/{repo}/branches/{branch}/topics/{path}", br + "/topics/a", true},
		{"/repos/{repo}/branches/{branch}/changes", br + "/changes", true},
		{"/repos/{repo}/branches/{branch}/commits", br + "/commits", true},
		{"/repos/{repo}/branches/{branch}/commits/{sha}", br + "/commits/" + sha, true},
		{"/repos/{repo}/branches/{branch}/commits/{sha}/facts/{path}", br + "/commits/" + sha + "/facts/kb/a/1.md", true},
		{"/repos/{repo}/branches/{branch}/commits/{sha}/topics", br + "/commits/" + sha + "/topics", false}, // 501: not yet supported
		{"/repos/{repo}/branches/{branch}/commits/{sha}/topics/{path}", br + "/commits/" + sha + "/topics/a", false},
		{"/repos/{repo}/branches/{branch}/activity", br + "/activity", true},
		{"/repos/{repo}/branches/{branch}/completions", br + "/completions?category=domain", true},
		{"/repos/{repo}/branches/{branch}/domains", br + "/domains", true},
		{"/repos/{repo}/branches/{branch}/domains/{name}", br + "/domains/d", true},
		{"/repos/{repo}/branches/{branch}/motifs", br + "/motifs", true},
		{"/repos/{repo}/branches/{branch}/motifs/{key}", br + "/motifs/config-drift", false},
		{"/repos/{repo}/branches/{branch}/stats", br + "/stats", true},
		{"/repos/{repo}/branches/{branch}/events", br + "/events", false}, // a stream: only refusals are read
		{"/repos/{repo}/branches/{branch}/triggers", br + "/triggers", true},
		{"/repos/{repo}/branches/{branch}/facts/{path}", br + "/facts/kb/a/1.md", true},
		{"/repos/{repo}/branches/{branch}/facts/{path}", br + "/facts/kb/a/1.md/incoming", true},
		{"/repos/{repo}/branches/{branch}/facts/{path}/commits", br + "/facts/kb/a/1.md/commits", true},
		{"/repos/{repo}/branches/{branch}/facts/{path}/materialize", br + "/facts/kb/a/1.md/materialize", true},
		{"/repos/{repo}/branches/{branch}/commits/{sha}/facts/{path}", br + "/commits/" + sha + "/facts/kb/a/1.md/outgoing", true},
		{"/lenses/{lens}/facts/{path}", lens + "/facts/kb/a/1.md", true},
		// the lens serves the repo route's /commits sub-resource (no spec path of its own)
		{"/repos/{repo}/branches/{branch}/facts/{path}/commits", lens + "/facts/kb/a/1.md/commits", true},
		{"/lenses/{lens}/completions", lens + "/completions?category=domain", true},
		{"/lenses/{lens}/motifs", lens + "/motifs", true},
		{"/lenses/{lens}/motifs/{key}", lens + "/motifs/config-drift", false},
		{"/lenses/{lens}/stats", lens + "/stats", true},
		{"/lenses/{lens}/topics", lens + "/topics", true},
		{"/lenses/{lens}/topics/{path}", lens + "/topics/a", true},
	}
	problem := func(body string) (string, string) {
		var p struct{ Title, Detail string }
		_ = json.Unmarshal([]byte(body), &p)
		return p.Title, p.Detail
	}
	with := func(u, q string) string {
		if strings.Contains(u, "?") {
			return u + "&" + q
		}
		return u + "?" + q
	}
	refusedForParam := func(code int, body string) bool {
		_, d := problem(body)
		return code == http.StatusBadRequest && (strings.HasPrefix(d, "unknown parameter") || strings.HasPrefix(d, "malformed query string"))
	}
	for _, rt := range routes {
		if rt.ok {
			code, body := restGet(t, h, rt.url)
			require.Equal(t, http.StatusOK, code, "%s bare: %s", rt.url, body)
		}
		// unknown: materialize keeps its own (older) detail
		code, body := restGet(t, h, with(rt.url, "bogus=1"))
		require.Equal(t, http.StatusBadRequest, code, "%s ?bogus: %s", rt.url, body)
		title, detail := problem(body)
		if strings.HasSuffix(rt.spec, "/materialize") {
			require.Contains(t, body, `unknown parameter \"bogus\"`, rt.url)
		} else {
			require.Equal(t, "Invalid parameter", title, body)
			require.Equal(t, `unknown parameter "bogus"`, detail, body)
		}
		for raw, want := range map[string]string{
			"%zz=1":     `malformed query string: invalid URL escape "%zz"`,
			"bogus;x=1": "malformed query string: invalid semicolon separator in query",
		} {
			code, body := restGet(t, h, with(rt.url, raw))
			require.Equal(t, http.StatusBadRequest, code, "%s ?%s: %s", rt.url, raw, body)
			title, detail := problem(body)
			require.Equal(t, "Invalid parameter", title, body)
			require.Equal(t, want, detail, body)
		}
		if strings.HasSuffix(rt.spec, "/events") {
			continue
		}
		for _, n := range specQueryParams(t, rt.spec) {
			v, ok := kbParamValue[n]
			require.True(t, ok, "no test value for %s on %s", n, rt.spec)
			code, body := restGet(t, h, with(rt.url, url.Values{n: {v}}.Encode()))
			require.False(t, refusedForParam(code, body), "%s ?%s=%s refused as a parameter: %s", rt.url, n, v, body)
		}
	}
}

// A raw system file is read as a file whatever its last segment is, so a
// .knomit/ path ending in /materialize or /commits takes the fact read's
// (empty) declared set, not the sub-resource's — on the repo, commit-anchored
// and lens fact routes alike.
//
// Sabotage: test the sub-resource suffix before the system-file decision in
// refuseUnknownFactPath (materialize's bogus reaches the file read, a 404;
// commits' limit is accepted, a 404).
func TestKBRoutes_SystemFilePathTakesTheFactReadSet(t *testing.T) {
	e := newLensRESTEnv(t, "alpha")
	alpha := e.m.Get("alpha")
	writeFactsInOneCommit(t, alpha, factSpec{path: "kb/a/1.md", title: "alpha one"})
	var sha string
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		var err error
		sha, err = svc.HeadCommit(context.Background(), alpha.AgentBranch())
		require.NoError(t, err)
	}))
	h := e.as(t, "reader")
	br := "/api/v1/repos/alpha/branches/" + urlBranch(alpha.AgentBranch())
	for _, base := range []string{br + "/facts", br + "/commits/" + sha + "/facts", "/api/v1/lenses/eng/facts"} {
		for path, q := range map[string]string{".knomit/x/materialize": "bogus=1", ".knomit/x/commits": "limit=5"} {
			u := base + "/" + path + "?" + q
			code, body := restGet(t, h, u)
			require.Equal(t, http.StatusBadRequest, code, "%s: %s", u, body)
			var p struct{ Title, Detail string }
			require.NoError(t, json.Unmarshal([]byte(body), &p), body)
			require.Equal(t, "Invalid parameter", p.Title, body)
			require.Equal(t, `unknown parameter "`+strings.SplitN(q, "=", 2)[0]+`"`, p.Detail, u)
		}
	}
}
