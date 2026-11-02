package manage

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"knomit/internal/edge/web/httpx"
)

// openAPIQueryParams is the query parameter names openapi.yaml declares on the
// GET of route ($refs to components/parameters resolved, path-level
// parameters included), or nil when the route has no GET there. A copy of the
// kb package's helper of the same name: a test file cannot be imported.
func openAPIQueryParams(t *testing.T, doc map[string]any, route string) []string {
	t.Helper()
	comps := doc["components"].(map[string]any)["parameters"].(map[string]any)
	item, ok := doc["paths"].(map[string]any)[route].(map[string]any)
	require.True(t, ok, "openapi.yaml has no %s", route)
	get, ok := item["get"].(map[string]any)
	require.True(t, ok, "openapi.yaml has no GET %s", route)
	ps, _ := get["parameters"].([]any)
	shared, _ := item["parameters"].([]any)
	out := []string{}
	for _, p := range append(ps, shared...) {
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

// manageGETRoutes is every GET route Mount registers, by its openapi.yaml
// path, with the declared set its handler refuses by. The web package's
// TestManageRoutes_UnknownAndMalformedAre400_DeclaredAccepted drives each one
// through the real router.
var manageGETRoutes = map[string]httpx.DeclaredParams{
	"/repo-events":                    repoEventsParams,
	"/fleet":                          fleetParams,
	"/fleet/members":                  fleetMembersParams,
	"/fleet/activity":                 fleetActivityParams,
	"/fleet/root":                     fleetRootParams,
	"/archived":                       archivedParams,
	"/archived/{id}":                  archivedItemParams,
	"/repos":                          reposParams,
	"/repo-creates":                   repoCreatesParams,
	"/repo-creates/{id}":              repoCreateParams,
	"/ontologies/presets":             ontologyPresetsParams,
	"/ontologies/presets/{name}":      ontologyPresetParams,
	"/ontologies/schema":              ontologySchemaParams,
	"/templates":                      templatesParams,
	"/repos/{repo}":                   repoParams,
	"/repos/{repo}/templates":         repoTemplatesParams,
	"/repos/{repo}/origin":            originParams,
	"/repos/{repo}/branches":          branchesParams,
	"/repos/{repo}/pushed-branches":   pushedBranchesParams,
	"/repos/{repo}/experiments":       experimentsParams,
	"/repos/{repo}/branches/{branch}": branchParams,
	"/lenses":                         lensesParams,
	"/lenses/{lens}":                  lensParams,
}

// Each management GET route's declared set IS its openapi.yaml query
// parameter list, and every such route has a GET in the spec: a parameter
// added to one side only, or a GET route the spec does not document, fails
// here.
//
// Sabotage: drop "limit" from fleetActivityParams (or from the spec's GET
// /fleet/activity) and that row differs; delete the spec's GET
// /repos/{repo}/experiments and its row fails.
func TestManageRouteParams_MatchOpenAPI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "static", "openapi.yaml"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	for route, d := range manageGETRoutes {
		require.Equal(t, openAPIQueryParams(t, doc, route), d.Names(), route)
	}
}
