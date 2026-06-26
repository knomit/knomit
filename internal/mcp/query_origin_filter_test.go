package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// originFactBody serializes a minimal fact body with the given type and origin.
// Used by the MCP origin-filter test to seed a heterogeneous corpus without
// going through LearnHandler (which only writes authored facts).
func originFactBody(title string, ty fact.Type, origin fact.Origin) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = "body"
	f.Type = ty
	f.Origin = origin
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"test"}
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// seedOriginFixture writes one fact of each origin (authored observation,
// distilled synthesis, discovered synthesis) directly into the test repo's
// store, bypassing LearnHandler so the discovery-engine surfaces (distilled,
// discovered) are exercisable end-to-end.
func seedOriginFixture(t *testing.T, ri *repos.RepoInstance, ctx context.Context) (authoredPath, distilledPath, discoveredPath string) {
	t.Helper()
	authoredPath = "kb/obs/a.md"
	distilledPath = "kb/synth/d.md"
	discoveredPath = "kb/synth/e.md"
	ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), authoredPath,
			originFactBody("Authored Obs", fact.Observation, fact.Authored),
			"add authored", "learn")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, ri.AgentBranch(), distilledPath,
			originFactBody("Distilled Synth", fact.Synthesis, fact.Distilled),
			"add distilled", "learn")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, ri.AgentBranch(), discoveredPath,
			originFactBody("Discovered Synth", fact.Synthesis, fact.Discovered),
			"add discovered", "learn")
		require.NoError(t, err)
	})
	return
}

// TestQuery_FiltersByOrigin_Discovered surfaces only the discovered fact when
// origin=["discovered"] is passed to knomit_query.
func TestQuery_FiltersByOrigin_Discovered(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	authoredPath, distilledPath, discoveredPath := seedOriginFixture(t, ri, ctx)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"origin": []any{"discovered"},
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "query should succeed; got: %s", resultText(t, result))

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Len(t, resp.Facts, 1, "origin=discovered must surface exactly the discovered fact")
	require.Equal(t, discoveredPath, resp.Facts[0].File)
	// And it must NOT include the others.
	for _, f := range resp.Facts {
		require.NotEqual(t, authoredPath, f.File)
		require.NotEqual(t, distilledPath, f.File)
	}
}

// TestQuery_FiltersByOrigin_AcceptsScalar verifies the handler accepts a bare
// string (not just an array) for origin, matching the schema's stringOrSlice
// coercion.
func TestQuery_FiltersByOrigin_AcceptsScalar(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	_, _, discoveredPath := seedOriginFixture(t, ri, ctx)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"origin": "discovered",
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "scalar origin must be accepted; got: %s", resultText(t, result))

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Len(t, resp.Facts, 1)
	require.Equal(t, discoveredPath, resp.Facts[0].File)
}

// TestQuery_Origin_AcceptedAsSoleFilter ensures the "at least one filter"
// validator accepts origin on its own — matching the precedent set by type
// and applies_to.
func TestQuery_Origin_AcceptedAsSoleFilter(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"origin": []any{"authored"},
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	if result.IsError {
		text := resultText(t, result)
		require.NotContains(t, text, "at least one of",
			"origin alone must satisfy the filter-required check; got %q", text)
	}
}
