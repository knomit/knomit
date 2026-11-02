package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// specQueryParams is the query parameter names static/openapi.yaml declares on
// the GET of route ($refs resolved).
func specQueryParams(t *testing.T, route string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("static", "openapi.yaml"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	comps := doc["components"].(map[string]any)["parameters"].(map[string]any)
	get := doc["paths"].(map[string]any)[route].(map[string]any)["get"].(map[string]any)
	var out []string
	for _, p := range get["parameters"].([]any) {
		m := p.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			m = comps[strings.TrimPrefix(ref, "#/components/parameters/")].(map[string]any)
		}
		if m["in"] == "query" {
			out = append(out, m["name"].(string))
		}
	}
	return out
}

// paramValue is a valid value for each declared list parameter.
func paramValue(t *testing.T, name, repo string) (string, string) {
	t.Helper()
	switch name {
	case "context.<key>":
		return "context.task", "t1"
	case "repo":
		return name, repo
	}
	v, ok := map[string]string{
		"q": "x", "query": "x", "entity": "E", "entities": "E", "domain": "d", "domain_exact": "true",
		"applies_to": "d", "path": "kb/", "topic": "t", "min_confidence": "0.5", "min_similarity": "0.1",
		"limit": "5", "offset": "0", "type": "observation", "exclude_type": "policy", "kind": "epistemic",
		"exclude_kind": "pragmatic", "origin": "authored", "ep": "learn", "motifs": "a-b", "motif_match": "exact",
		"expired": "false", "expires_before": "2030-01-01T00:00:00Z", "expires_after": "2020-01-01T00:00:00Z",
		"since_fork": "1", "sort": "recent",
	}[name]
	require.True(t, ok, "no test value for declared parameter %q", name)
	return name, v
}

// The four list routes answer 400 "unknown parameter" to a query parameter
// they do not declare, and accept every parameter openapi.yaml declares on
// them, aliases and paging included (q/query, entity/entities, limit,
// offset). since_fork is declared but a branch that is not an experiment
// answers it 400 "Not an experiment" — accepted as a parameter, refused for
// the branch.
//
// Sabotage: remove the refuseUnknown call from one handler (its row answers
// 200 to the unknown parameter); drop a declared name from a route's set
// (that name's request is the unknown-parameter 400).
func TestListRoutes_UnknownParamIs400_DeclaredParamsAccepted(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	h := e.as(t, "reader")
	repoBase := "/api/v1/repos/alpha/branches/" + urlBranch(e.m.Get("alpha").AgentBranch())
	routes := []struct{ spec, url string }{
		{"/repos/{repo}/branches/{branch}/search", repoBase + "/search"},
		{"/repos/{repo}/branches/{branch}/facts", repoBase + "/facts"},
		{"/lenses/{lens}/search", "/api/v1/lenses/eng/search"},
		{"/lenses/{lens}/facts", "/api/v1/lenses/eng/facts"},
	}
	problem := func(body string) (string, string) {
		var p struct{ Title, Detail string }
		_ = json.Unmarshal([]byte(body), &p)
		return p.Title, p.Detail
	}
	for _, rt := range routes {
		// unknown: one never-declared name, and a REST alias of another route
		for _, bogus := range []string{"bogus", "cursor"} {
			code, body := restGet(t, h, rt.url+"?"+bogus+"=1")
			require.Equal(t, http.StatusBadRequest, code, "%s ?%s: %s", rt.url, bogus, body)
			title, detail := problem(body)
			require.Equal(t, "Invalid parameter", title, body)
			require.Equal(t, `unknown parameter "`+bogus+`"`, detail, body)
		}
		// a declared one beside an unknown one is still refused
		code, body := restGet(t, h, rt.url+"?q=x&zzz=1")
		require.Equal(t, http.StatusBadRequest, code, body)
		_, detail := problem(body)
		require.Equal(t, `unknown parameter "zzz"`, detail)

		names := specQueryParams(t, rt.spec)
		require.NotEmpty(t, names)
		all := url.Values{}
		for _, n := range names {
			k, v := paramValue(t, n, "beta")
			code, body := restGet(t, h, rt.url+"?"+url.Values{k: {v}}.Encode())
			if n == "since_fork" {
				require.Equal(t, http.StatusBadRequest, code, body)
				_, detail := problem(body)
				require.NotContains(t, detail, "unknown parameter", "%s since_fork: %s", rt.url, body)
				continue
			}
			require.Equal(t, http.StatusOK, code, "%s ?%s=%s: %s", rt.url, k, v, body)
			all.Set(k, v)
		}
		// every declared parameter at once (since_fork aside)
		code, body = restGet(t, h, rt.url+"?"+all.Encode())
		require.Equal(t, http.StatusOK, code, "%s with every declared parameter: %s", rt.url, body)
	}
}

// Repo /facts declares sort with the one value its order has: recent is
// accepted, anything else is a 400 (never silently the newest-first list).
//
// Sabotage: accept any sort value (sort=relevance answers 200).
func TestRepoFacts_SortOnlyRecent(t *testing.T) {
	e := newLensRESTEnv(t, "alpha")
	h := e.as(t, "reader")
	u := "/api/v1/repos/alpha/branches/" + urlBranch(e.m.Get("alpha").AgentBranch()) + "/facts"
	code, body := restGet(t, h, u+"?sort=recent")
	require.Equal(t, http.StatusOK, code, body)
	code, body = restGet(t, h, u+"?sort=relevance")
	require.Equal(t, http.StatusBadRequest, code, body)
	require.Contains(t, body, `sort must be \"recent\"`)
}
