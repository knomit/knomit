package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// principlesLearnRequest builds a CallToolRequest that drives LearnHandler
// against the principles topic. Tests tweak individual fields to target a
// specific validation rule. The base values are chosen so that — when used
// unmodified — the fact passes all four principles rules.
func principlesLearnRequest(momentName string, fields map[string]any) mcpgo.CallToolRequest {
	factFields := map[string]any{
		"topic":      "principles",
		"category":   "mission/foo",
		"title":      "Test Principle",
		"body":       "designer authored this principle.",
		"kind":       "pragmatic",
		"type":       "policy",
		"domain":     []any{"global"},
		"confidence": 0.8,
		"sources":    1,
		"entities":   []any{"designer"},
		"refs":       []any{},
	}
	for k, v := range fields {
		factFields[k] = v
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": momentName,
		"facts":       []any{factFields},
	}
	return req
}

// runPrinciplesLearn wires the real CodeOntology into a fresh test repo and
// invokes LearnHandler with the supplied request.
func runPrinciplesLearn(t *testing.T, req mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// TestPrinciples_MissingDesignerRejected exercises rule
// `must-have-designer-entity`: entities lacks 'designer'.
func TestPrinciples_MissingDesignerRejected(t *testing.T) {
	req := principlesLearnRequest("missing-designer", map[string]any{
		"entities": []any{},
	})
	result := runPrinciplesLearn(t, req)
	require.True(t, result.IsError, "expected rejection; got success")
	text := resultText(t, result)
	require.Contains(t, text, "must-have-designer-entity",
		"error must reference the failing rule; got %q", text)
}

// TestPrinciples_NonPragmaticPolicyRejected exercises rule
// `must-be-pragmatic-policy`: kind=pragmatic but type=heuristic (a valid
// pragmatic type that is NOT policy). 'designer' is still present so the
// first rule passes.
func TestPrinciples_NonPragmaticPolicyRejected(t *testing.T) {
	req := principlesLearnRequest("non-pragmatic-policy", map[string]any{
		"kind": "pragmatic",
		"type": "heuristic",
	})
	result := runPrinciplesLearn(t, req)
	require.True(t, result.IsError, "expected rejection; got success")
	text := resultText(t, result)
	require.Contains(t, text, "must-be-pragmatic-policy",
		"error must reference the failing rule; got %q", text)
}

// TestPrinciples_DomainGlobalAndAreaRejected exercises rule
// `domain-mutually-exclusive`: domain contains both 'global' and another
// entry. All earlier rules pass.
func TestPrinciples_DomainGlobalAndAreaRejected(t *testing.T) {
	req := principlesLearnRequest("domain-mixed", map[string]any{
		"domain": []any{"global", "store"},
	})
	result := runPrinciplesLearn(t, req)
	require.True(t, result.IsError, "expected rejection; got success")
	text := resultText(t, result)
	require.Contains(t, text, "domain-mutually-exclusive",
		"error must reference the failing rule; got %q", text)
}

// TestPrinciples_EmptyDomainRejected exercises rule `domain-non-empty`:
// domain is the empty list. All earlier rules pass.
func TestPrinciples_EmptyDomainRejected(t *testing.T) {
	req := principlesLearnRequest("empty-domain", map[string]any{
		"domain": []any{},
	})
	result := runPrinciplesLearn(t, req)
	require.True(t, result.IsError, "expected rejection; got success")
	text := resultText(t, result)
	require.Contains(t, text, "domain-non-empty",
		"error must reference the failing rule; got %q", text)
}

// TestPrinciples_ValidPrincipleAccepted is the canonical happy path:
// kind=pragmatic, type=policy, domain=['global'], entities=['designer'].
// All four rules pass.
func TestPrinciples_ValidPrincipleAccepted(t *testing.T) {
	req := principlesLearnRequest("valid-principle", nil)
	result := runPrinciplesLearn(t, req)
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", resultText(t, result))
	}
	text := resultText(t, result)
	require.Contains(t, text, "kb/principles/mission/foo/",
		"response should include the written fact path; got %q", text)
}
