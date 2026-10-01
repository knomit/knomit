package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// #384: knomit_learn with a control character in a segment that becomes a
// tree path committed a path go-git cannot read back, moved the ref, and then
// failed the index sync. Every later write on the branch did the same, so the
// index froze. The store now refuses the path before the ref moves.

func controlCharLearnReq(moment string, fact map[string]any) mcpgo.CallToolRequest {
	f := map[string]any{"body": "b " + moment, "confidence": 0.8, "sources": 1}
	for k, v := range fact {
		f[k] = v
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": moment, "facts": []any{f}}
	return req
}

func TestLearn_ControlCharacterInPathSegment_RefusedBeforeCommit(t *testing.T) {
	cases := map[string]map[string]any{
		"category": {"topic": "architecture", "category": "a\nb", "title": "T"},
		"topic":    {"topic": "arch\nitecture", "category": "cat", "title": "T"},
		"path":     {"path": ".knomit/jobs/a\nb/x.md", "title": "T"},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			ri := newLearnTestRepo(t, nil)
			ctx := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

			first, err := LearnHandler()(ctx, controlCharLearnReq("m0",
				map[string]any{"topic": "architecture", "category": "first/cat", "title": "First"}))
			require.NoError(t, err)
			require.False(t, first.IsError, resultText(t, first))

			before := headOf(t, ri, "agent/test")
			res, err := LearnHandler()(ctx, controlCharLearnReq("m", bad))
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Equal(t, before, headOf(t, ri, "agent/test"), "a refused learn must not move the ref")
			text := resultText(t, res)
			// The store's message reaches the caller intact: the path quoted,
			// so the line feed shows as \n instead of breaking the line.
			require.Contains(t, text, `invalid path "`)
			require.Contains(t, text, `\n`)
			require.Contains(t, text, "contains a control character")
			require.NotContains(t, text, "\n")
			require.NotContains(t, text, "notifyCommit", "refused before the commit, not by the index sync after it")

			// The branch is not poisoned: the next write succeeds and is indexed.
			good, err := LearnHandler()(ctx, controlCharLearnReq("m2",
				map[string]any{"topic": "architecture", "category": "ok/cat", "title": "Good"}))
			require.NoError(t, err)
			require.False(t, good.IsError, resultText(t, good))

			out, qres := queryFacts(t, ctx, map[string]any{"path": "kb/architecture"})
			require.False(t, qres.IsError, resultText(t, qres))
			titles := make([]string, 0, len(out.Facts))
			for _, f := range out.Facts {
				titles = append(titles, f.Title)
			}
			require.ElementsMatch(t, []string{"First", "Good"}, titles)
		})
	}
}
