package kb

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// openAPIQueryParams is the query parameter names openapi.yaml declares on the
// GET of route, $refs to components/parameters resolved.
func openAPIQueryParams(t *testing.T, route string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "static", "openapi.yaml"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	comps := doc["components"].(map[string]any)["parameters"].(map[string]any)
	item, ok := doc["paths"].(map[string]any)[route].(map[string]any)
	require.True(t, ok, "openapi.yaml has no %s", route)
	get := item["get"].(map[string]any)
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
	sort.Strings(out)
	return out
}

// names renders d back into OpenAPI's spelling, sorted.
func (d declaredParams) list() []string {
	var out []string
	for n := range d.names {
		out = append(out, n)
	}
	for _, p := range d.prefixes {
		out = append(out, p+"<key>")
	}
	sort.Strings(out)
	return out
}

// Each list route's code-side declared set IS its OpenAPI parameter list: a
// parameter added to the handler's set and not to the spec, or the other way
// round, fails here, so the 400 a client gets and the reference it reads
// cannot disagree.
//
// Sabotage: drop "sort" from repoFactsParams (or since_fork from the spec)
// and the repo /facts row differs.
func TestListRouteParams_MatchOpenAPI(t *testing.T) {
	for route, d := range map[string]declaredParams{
		"/repos/{repo}/branches/{branch}/search": repoSearchParams,
		"/repos/{repo}/branches/{branch}/facts":  repoFactsParams,
		"/lenses/{lens}/search":                  lensSearchParams,
		"/lenses/{lens}/facts":                   lensFactsParams,
	} {
		require.Equal(t, openAPIQueryParams(t, route), d.list(), route)
	}
}

// A family declares only its members: "context." alone, or a name that only
// shares a stem, is unknown.
func TestDeclaredParams_FamilyPrefix(t *testing.T) {
	d := declare("q", "context.<key>")
	require.True(t, d.has("q"))
	require.True(t, d.has("context.task"))
	require.False(t, d.has("context."))
	require.False(t, d.has("contextual"))
	require.False(t, d.has("qq"))
}
