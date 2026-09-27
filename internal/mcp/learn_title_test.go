package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// knomit_learn must refuse a fact with no usable title, naming the fact by
// index, and write NOTHING from the batch — the valid fact beside it included.
func TestLearnHandler_RejectsMissingOrBlankTitle(t *testing.T) {
	cases := map[string]map[string]any{
		"absent":     {},
		"empty":      {"title": ""},
		"whitespace": {"title": "  \t "},
	}
	for name, titleField := range cases {
		t.Run(name, func(t *testing.T) {
			svc, ctx, emb := newPrinciplesTestRepo(t)
			before := headCommit(t, svc)

			bad := map[string]any{
				"topic": "gotchas", "category": "testing/titles",
				"body": "a body with no title", "entities": []any{"x"},
			}
			for k, v := range titleField {
				bad[k] = v
			}
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{
				"moment_name": "blank-title",
				"facts": []any{
					map[string]any{
						"topic": "gotchas", "category": "testing/titles",
						"title": "A fine title", "body": "a valid fact", "entities": []any{"y"},
					},
					bad,
				},
			}
			res, err := LearnHandler(emb)(ctx, req)
			require.NoError(t, err)
			require.True(t, res.IsError, "a blank title must fail the call")
			require.Contains(t, resultText(t, res), "fact 1: title is required")
			require.Equal(t, before, headCommit(t, svc), "the batch is all-or-nothing: nothing may be written")
		})
	}
}

var _ = context.Background
