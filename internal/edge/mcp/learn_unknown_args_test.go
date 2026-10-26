package mcp

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// A top-level argument knomit_learn does not declare is refused, like every
// other tool's. Before: only the per-fact keys were checked, so a top-level
// distinct_from was ignored, the dedup merge folded the fact into the very path
// the caller meant to keep apart, and the call reported success.
//
// The control is the same call with distinct_from on the fact, which must land
// at its own path: it proves the refused call would otherwise have merged.
//
// SABOTAGE: drop the rejectUnknownArguments call in LearnHandler → the call
// succeeds, merges, and every assertion below goes red.
func TestLearnHandler_RejectsUnknownTopLevelArgument(t *testing.T) {
	for _, tc := range []struct {
		key      string
		value    any
		contains []string
	}{
		{"distinct_from", []any{"x"}, []string{`unknown argument "distinct_from"`, "belongs on each fact"}},
		{"bogus", "v", []string{`unknown argument "bogus"`}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			ri, ctx, emb := newDedupAttrRepo(t)
			svc := testRepoService(t, ctx)
			seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Same claim", "observation")))[0]
			value := tc.value
			if tc.key == "distinct_from" {
				value = []any{seed}
			}
			before := liveFactCount(t, svc)
			headBefore := settledMessage(t, ri, "agent/test")

			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{
				"moment_name": "again",
				"facts":       []any{settledFact("notes", "a", "Same claim", "observation")},
				tc.key:        value,
			}
			r, err := LearnHandler(emb)(ctx, req)
			require.NoError(t, err)
			require.True(t, r.IsError, "an undeclared top-level argument is refused: %s", resultText(t, r))
			for _, c := range tc.contains {
				require.Contains(t, resultText(t, r), c)
			}
			require.Equal(t, before, liveFactCount(t, svc), "a refused call writes nothing")
			require.Equal(t, headBefore, settledMessage(t, ri, "agent/test"), "no commit was made")
		})
	}

	t.Run("control: per-fact distinct_from is accepted and keeps the fact apart", func(t *testing.T) {
		_, ctx, emb := newDedupAttrRepo(t)
		seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Same claim", "observation")))[0]
		res := settledLearn(t, ctx, emb, "again", nil, settledFact("notes", "a", "Same claim", "observation", seed))
		require.NotEqual(t, []string{seed}, settledWritten(t, res), "written at its own path")
	})
}
