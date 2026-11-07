package mcp

import (
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// The user-approved wording of knomit_learn's validation refusals, asserted as
// whole strings. <topics> is the repo's ontology, read at runtime; the real
// ontology here is fact.CodeOntology().
func TestLearnHandler_ValidationMessages(t *testing.T) {
	topics := strings.Join(fact.CodeOntology().TopicNames(), ", ")
	// Pin the literal too, so a change to the ontology is a visible edit here.
	require.Equal(t, "architecture, conventions, decisions, gotchas, incidents, invariants, meta, principles", topics)

	good := func(over map[string]any) map[string]any {
		f := map[string]any{"title": "T", "body": "b", "entities": []any{"x"}}
		for k, v := range over {
			if v == nil {
				delete(f, k)
			} else {
				f[k] = v
			}
		}
		return f
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"topic missing", map[string]any{"facts": []any{good(map[string]any{"category": "a/b"})}},
			"fact 0: topic is missing (or give path for an artifact); valid: " + topics},
		{"topic unknown", map[string]any{"facts": []any{good(map[string]any{"topic": "chain", "category": "a"})}},
			`fact 0: unknown topic "chain"; valid: ` + topics},
		{"neither topic/category nor path", map[string]any{"facts": []any{good(nil)}},
			"fact 0: topic and category are missing (or give path for an artifact); valid topics: " + topics},
		{"category missing", map[string]any{"facts": []any{good(map[string]any{"topic": "gotchas"})}},
			"fact 0: category is missing"},
		{"path and topic/category", map[string]any{"facts": []any{good(map[string]any{"path": "artifacts/a/b.md", "topic": "gotchas", "category": "c"})}},
			"fact 0: give topic and category, or path, not both"},
		{"bad path", map[string]any{"facts": []any{good(map[string]any{"path": "artifacts/x"})}},
			`fact 0: path "artifacts/x" is not an artifact path (artifacts/<area>/<name>.md)`},
		{"title missing", map[string]any{"facts": []any{good(map[string]any{"topic": "gotchas", "category": "c", "title": nil})}},
			"fact 0: title is missing"},
		{"facts empty", map[string]any{"facts": []any{}},
			"facts is empty (unless retract is given)"},
		{"moment_name missing", map[string]any{"moment_name": "", "facts": []any{good(map[string]any{"topic": "gotchas", "category": "c"})}},
			"moment_name is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, emb := newPrinciplesTestRepo(t)
			before := headCommit(t, svc)
			var req mcpgo.CallToolRequest
			args := map[string]any{"moment_name": "m"}
			for k, v := range tc.args {
				args[k] = v
			}
			req.Params.Arguments = args
			res, err := LearnHandler(emb)(ctx, req)
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Equal(t, tc.want, resultText(t, res))
			require.Equal(t, before, headCommit(t, svc))
		})
	}
}

func TestLearnToolSchema_TopicCategoryDescriptions(t *testing.T) {
	p := learnToolSchemaProperties()
	require.Equal(t, "This repo's ontology topic. Required unless you give path.",
		p["topic"].(map[string]any)["description"])
	require.Equal(t, "Category path within the topic. Required unless you give path.",
		p["category"].(map[string]any)["description"])
}
