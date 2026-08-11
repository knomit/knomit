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

// rootValidationOntologyYAML declares a validation at the ONTOLOGY ROOT rather
// than under a topic. fact.ValidateFact runs root rules UNCONDITIONALLY, before
// it ever looks the topic up, so a root rule is the one kind of rule that fires
// for a path with no ontology placement at all — which is exactly what a
// private-state fact under .knomit/<area>/ is.
//
// `validations:` at the top level is an authorable ontology field
// (internal/fact/ontology.go). No shipped preset uses one today, which is the
// only reason the update path's missing guard was invisible.
const rootValidationOntologyYAML = `id: t
name: T
validations:
  - name: must-have-designer
    message: "every fact must name the designer entity"
    rule: "fact.entities.includes('designer')"
topics:
  principles:
    description: x
    children:
      mission:
        description: x
`

func rootValidationCtx(t *testing.T) context.Context {
	t.Helper()
	ontology, err := fact.ParseOntology([]byte(rootValidationOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)
	return repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
}

// learnKBFact writes one ordinary kb/ fact and returns the raw result.
func learnKBFact(t *testing.T, ctx context.Context, title string, entities []any) *mcpgo.CallToolResult {
	t.Helper()
	return callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "kb-write",
		"facts": []any{map[string]any{
			"topic":      "principles",
			"category":   "mission",
			"title":      title,
			"body":       "b",
			"kind":       "pragmatic",
			"type":       "policy",
			"domain":     []any{},
			"confidence": 0.9,
			"sources":    1,
			"entities":   entities,
			"refs":       []any{},
		}},
	})
}

// learnedPath pulls the single written fact path out of a learn result.
func learnedPath(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	var out struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	require.Len(t, out.Commits, 1)
	return out.Commits[0].File
}

// TestRootValidations_DoNotApplyToPrivateState is the cross-tool seam: a
// private-state slot must be learnABLE and updatABLE under an ontology whose
// root rules its envelope cannot satisfy.
//
// knomit_learn guards its ValidateFact call on "this fact has an ontology
// placement". knomit_update derives a topic path by stripping the ontology root
// — a no-op for .knomit/<area>/… — and hands ValidateFact something like
// ".knomit/jobs/ae". The unknown topic makes the per-topic walk a no-op, but the
// ROOT rules already ran. A job could therefore allocate its slot on run one and
// have every subsequent update refused.
func TestRootValidations_DoNotApplyToPrivateState(t *testing.T) {
	ctx := rootValidationCtx(t)

	// learn: already guarded today.
	result := learnAtPath(t, ctx, jobSlot, "crawl-state", "run 1")
	require.Falsef(t, result.IsError, "learn on a private slot must not run root rules: %s", resultText(t, result))

	// update: the regression. Every run after the first goes through here.
	result = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file":        jobSlot,
		"moment_name": "job-run",
		"updates":     map[string]any{"body": "run 2"},
	})
	require.Falsef(t, result.IsError, "update on a private slot must not run root rules: %s", resultText(t, result))
}

// TestRootValidations_StillApplyToKnowledge proves the rule above genuinely
// FIRES. A test that passes because the rule never ran would be worthless: the
// same ontology must still reject an ordinary kb/ fact that violates it, on
// both write paths.
func TestRootValidations_StillApplyToKnowledge(t *testing.T) {
	ctx := rootValidationCtx(t)

	// learn: a kb/ fact missing the entity is refused.
	result := learnKBFact(t, ctx, "No Designer", []any{})
	require.True(t, result.IsError, "root rule must reject a kb/ fact that violates it")
	require.Contains(t, resultText(t, result), "must-have-designer")

	// update: writing a kb/ fact into violation is refused too, so the fix
	// cannot have relaxed root validation for knowledge.
	result = learnKBFact(t, ctx, "With Designer", []any{"designer"})
	require.Falsef(t, result.IsError, "a conforming kb/ fact must be writable: %s", resultText(t, result))
	path := learnedPath(t, result)

	result = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file":        path,
		"moment_name": "m",
		"updates":     map[string]any{"entities": []any{}},
	})
	require.True(t, result.IsError, "root rule must reject a kb/ update that violates it")
	require.Contains(t, resultText(t, result), "must-have-designer")
}
