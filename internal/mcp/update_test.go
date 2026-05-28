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

// seedValidPrinciple writes a fact that satisfies the must-have-designer
// rule via LearnHandler, and returns the path it was written to. It is a
// helper for update tests that need a pre-existing fact to mutate.
func seedValidPrinciple(t *testing.T, ctx context.Context) string {
	t.Helper()

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "seed",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Seeded Principle",
				"body":       "designer authored this.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}

	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "seed must succeed; got error: %s", resultText(t, result))

	var payload struct {
		Commits []struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &payload))
	require.Len(t, payload.Commits, 1)
	require.NotEmpty(t, payload.Commits[0].File)
	return payload.Commits[0].File
}

// TestUpdateHandler_RejectsFailingValidation seeds a valid principle fact,
// then attempts to update it in a way that violates the
// must-have-designer rule (drop the 'designer' entity). UpdateHandler must
// reject the change with an error referencing both the rule name and the
// rule's message.
func TestUpdateHandler_RejectsFailingValidation(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)
	ctx := repos.WithRepoInstance(context.Background(), ri)

	path := seedValidPrinciple(t, ctx)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": "drop-designer",
		"updates": map[string]any{
			"entities": []any{},
		},
	}

	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "expected validation failure to surface as IsError result")

	text := resultText(t, result)
	require.Contains(t, text, "must-have-designer",
		"error must reference the rule name; got %q", text)
	require.Contains(t, text, "/knomit-principle",
		"error must include the rule's message; got %q", text)
}
