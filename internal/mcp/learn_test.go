package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// principlesOntologyYAML is a tiny ontology with one validation rule that
// enforces 'designer' must be present in fact.entities. It is reused by the
// two validation tests below.
const principlesOntologyYAML = `id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: must-have-designer
        message: "principles must be authored via /knomit-principle"
        rule: "fact.entities.includes('designer')"
    children:
      mission:
        description: x
`

// newLearnTestRepo opens a fresh on-disk store, initialises the agent branch,
// and returns a RepoInstance wired with the given ontology. The OntologyRoot
// is "kb" — matching the handler's expectation that BuildFactPath uses that
// prefix.
func newLearnTestRepo(t *testing.T, ontology *fact.Ontology) *repos.RepoInstance {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     ontology,
		OntologyRoot: "kb",
	})
}

// resultText extracts the first TextContent string from an MCP result.
func resultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("result has no TextContent: %+v", result.Content)
	return ""
}

// TestLearnHandler_RejectsFailingValidation seeds a repo with an ontology
// that requires entities to include 'designer', then attempts to write a
// fact without that entity. The handler must reject the write with an
// error referencing the rule name AND the rule's message.
func TestLearnHandler_RejectsFailingValidation(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)

	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "reject-test",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Bad Principle",
				"body":       "missing designer entity.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{},
				"refs":       []any{},
			},
		},
	}

	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "expected validation failure to surface as IsError result")

	text := resultText(t, result)
	require.Contains(t, text, "must-have-designer",
		"error must reference the rule name; got %q", text)
	require.Contains(t, text, "/knomit-principle",
		"error must include the rule's message; got %q", text)
}

// TestLearnHandler_AcceptsValidFact seeds the same ontology and writes a
// fact that satisfies the rule. The write must succeed (no error).
func TestLearnHandler_AcceptsValidFact(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)

	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "accept-test",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Good Principle",
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
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", resultText(t, result))
	}

	// Sanity: response must contain at least one committed file path under
	// the expected category directory.
	text := resultText(t, result)
	require.Contains(t, text, "kb/principles/mission/foo/",
		"response should include the written fact path; got %q", text)
}
