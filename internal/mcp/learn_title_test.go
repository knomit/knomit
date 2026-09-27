package mcp

import (
	"context"
	"reflect"
	"sort"
	"strings"
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

// An unknown key inside a fact object is refused, not ignored — naming the
// fact, every unknown key, and the accepted set — and the batch writes nothing.
func TestLearnHandler_RejectsUnknownFactKeys(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	before := headCommit(t, svc)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "unknown-keys",
		"facts": []any{
			map[string]any{
				"topic": "gotchas", "category": "testing/keys",
				"title": "A fine fact", "body": "valid", "entities": []any{"y"},
			},
			map[string]any{
				"topic": "gotchas", "category": "testing/keys",
				"title": "Carries typos", "body": "b", "entities": []any{"x"},
				"tags": []any{"x"}, "confidense": 0.9,
			},
		},
	}
	res, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError, "an unknown fact key must fail the call")
	text := resultText(t, res)
	require.Contains(t, text, `fact 1: unknown key "confidense", "tags"`)
	require.Contains(t, text, "valid keys are: body, category, confidence")
	require.Equal(t, before, headCommit(t, svc), "the batch is all-or-nothing: nothing may be written")
}

// learnFactInput's json tags and the served fact schema are one set declared
// twice; the pin keeps a schema-declared key from being refused.
func TestLearnFactInput_MatchesServedSchema(t *testing.T) {
	var fromSchema []string
	for k := range learnToolSchemaProperties() {
		fromSchema = append(fromSchema, k)
	}
	var fromStruct []string
	rt := reflect.TypeOf(learnFactInput{})
	for i := range rt.NumField() {
		fromStruct = append(fromStruct, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(fromSchema)
	sort.Strings(fromStruct)
	require.Equal(t, fromSchema, fromStruct)
}
