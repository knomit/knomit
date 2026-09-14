package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// servedOpenAPI parses the spec the server actually hands a client, not the
// file on disk. The two can only differ through an embed/serve bug, but this is
// the artifact every consumer generates from.
func servedOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	s := &Server{Manager: newTestManagerWithRepos(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(rec.Body.Bytes(), &doc), "the served spec must be valid YAML")
	return doc
}

// The Repo schema's `required` list is the machine-readable half of a promise
// the prose and the handler both make: repoView emits read_branch on every
// repo, subscription or not. A generated client that treats it as optional
// would make every caller nil-check a field that is always there — and the
// drift would be invisible, since the description says "always present".
//
// agent_branch is deliberately NOT required: it is omitted for a subscription,
// which has none.
func TestOpenAPI_RepoSchemaRequiresReadBranch(t *testing.T) {
	doc := servedOpenAPI(t)

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	repo := schemas["Repo"].(map[string]any)

	required := make([]string, 0, 8)
	for _, v := range repo["required"].([]any) {
		required = append(required, v.(string))
	}
	require.Contains(t, required, "read_branch",
		"repoView always emits read_branch; the schema must say so. required=%v", required)
	require.NotContains(t, required, "agent_branch",
		"agent_branch is omitted for a subscription and must stay optional")

	// And the properties the subscribe work added are declared at all.
	props := repo["properties"].(map[string]any)
	for _, p := range []string{"read_branch", "mode", "agent_branch"} {
		require.Contains(t, props, p, "Repo schema is missing property %q", p)
	}
}
