package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// seedPrincipleWithDomain writes a single principle-shaped fact (kind=pragmatic,
// type=policy, entities=[designer]) at the requested domain via LearnHandler.
// It returns the full kb-relative path the fact was written to so callers can
// assert membership in subsequent query results.
func seedPrincipleWithDomain(t *testing.T, ctx context.Context, momentName, category, title, domain string) string {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": momentName,
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   category,
				"title":      title,
				"body":       "designer authored this principle.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{domain},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	if result.IsError {
		t.Fatalf("seed failed: %s", resultText(t, result))
	}

	// Parse the response to recover the written fact's path. The learn handler
	// returns JSON with a "files" or similar key; rather than coupling to the
	// exact response schema, walk the parsed structure and pick the first
	// string under kb/principles/<category>/.
	text := resultText(t, result)
	prefix := "kb/principles/" + category + "/"
	path := findPathWithPrefix(text, prefix)
	require.NotEmpty(t, path, "could not locate written fact path with prefix %q in response: %s", prefix, text)
	return path
}

// findPathWithPrefix walks any JSON structure looking for the first string
// value that begins with the given prefix. Keeps the test resilient to small
// shape changes in the learn handler response.
func findPathWithPrefix(jsonText, prefix string) string {
	var raw any
	if err := json.Unmarshal([]byte(jsonText), &raw); err != nil {
		return ""
	}
	return walkForPrefix(raw, prefix)
}

func walkForPrefix(v any, prefix string) string {
	switch x := v.(type) {
	case string:
		if len(x) >= len(prefix) && x[:len(prefix)] == prefix {
			return x
		}
	case map[string]any:
		for _, vv := range x {
			if got := walkForPrefix(vv, prefix); got != "" {
				return got
			}
		}
	case []any:
		for _, vv := range x {
			if got := walkForPrefix(vv, prefix); got != "" {
				return got
			}
		}
	}
	return ""
}

// TestQuery_AppliesTo_FiltersByAncestorMatch seeds two principle facts at
// different domains (store and ui), then queries with applies_to=[store/resolver].
// Only the store-scoped fact should appear because ancestor-or-equal match
// includes store but not ui.
func TestQuery_AppliesTo_FiltersByAncestorMatch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	storePath := seedPrincipleWithDomain(t, ctx, "seed-store", "mission/store", "Store Principle", "store")
	uiPath := seedPrincipleWithDomain(t, ctx, "seed-ui", "mission/ui", "UI Principle", "ui")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"applies_to": []any{"store/resolver"},
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "query should succeed; got: %s", resultText(t, result))

	text := resultText(t, result)
	require.Contains(t, text, storePath,
		"applies_to=[store/resolver] must surface the store-scoped fact; got %q", text)
	require.NotContains(t, text, uiPath,
		"applies_to=[store/resolver] must NOT surface the ui-scoped fact; got %q", text)
}

// TestQuery_AppliesTo_AcceptedAsSoleFilter ensures the "at least one filter"
// validator accepts applies_to on its own.
func TestQuery_AppliesTo_AcceptedAsSoleFilter(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"applies_to": []any{"x"},
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	if result.IsError {
		text := resultText(t, result)
		require.NotContains(t, text, "at least one of",
			"applies_to alone must satisfy the filter-required check; got %q", text)
	}
}

// TestQuery_SurfacesScoreAndRespectsLimit pins that knomit_query returns the
// relevance score per fact and honours a caller-supplied limit (was hardcoded 20).
func TestQuery_SurfacesScoreAndRespectsLimit(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	seedPrincipleWithDomain(t, ctx, "seed-a", "mission/store", "Alpha Store Principle", "store")
	seedPrincipleWithDomain(t, ctx, "seed-b", "mission/store", "Beta Store Principle", "store")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"text":  "store principle",
		"limit": 1,
	}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "query should succeed; got: %s", resultText(t, result))

	var out struct {
		Facts []struct {
			Score float64 `json:"score"`
		} `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	require.Len(t, out.Facts, 1, "limit=1 must cap results to 1")
	require.Greater(t, out.Facts[0].Score, 0.0, "score must be surfaced (non-zero)")
}

// TestQuery_FiltersByType pins the new `type` knob: a policy-typed fact is
// excluded when type=["observation"] and included when type=["policy"].
func TestQuery_FiltersByType(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	seedPrincipleWithDomain(t, ctx, "seed-typed", "mission/store", "Typed Store Principle", "store")

	query := func(typ string) int {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{"text": "store principle", "type": []any{typ}}
		result, err := QueryHandler()(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, "query should succeed; got: %s", resultText(t, result))
		var out struct {
			Facts []json.RawMessage `json:"facts"`
		}
		require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
		return len(out.Facts)
	}
	require.Zero(t, query("observation"), "type=observation must exclude the policy fact")
	require.Positive(t, query("policy"), "type=policy must include the policy fact")
}
