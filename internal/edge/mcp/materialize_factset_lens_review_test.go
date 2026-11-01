package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/auth"
)

// A lens FactSet over MCP embeds its text query ONCE for the whole fan-out
// (review L1), with the same embedder knomit_query on the lens uses
// (fanoutQueryVec over the server embedder), so its roots rank against the
// lens list's single vector. Both mounts' stores embed with the same counting
// embedder, so a per-mount embed is counted too: given the server embedder
// the count is 1; without it each mount embeds for itself (2), the
// positive control that the meter sees per-mount inferences at all.
//
// Sabotage (the lens FactSet passes no Embed): 2 inferences with the
// embedder handed in.
func TestFactSet_Lens_MCPEmbedsTheQueryOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hoist bool
		want  int64
	}{{"server embedder", true, 1}, {"no server embedder (per mount)", false, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			var meter atomic.Int64
			emb := countingFedEmbedder{queries: &meter}
			repoA, ctxA := countingFedRepo(t, emb)
			repoB, ctxB := countingFedRepo(t, emb)
			seedFedFact(t, ctxA, "seed-a", "mission/store", "alpha fact", "store", nil)
			seedFedFact(t, ctxB, "seed-b", "mission/store", "beta fact", "store", nil)
			b := repos.NewBindingForTest(repoA,
				repos.ReadTarget{RI: repoA, Branch: "agent/test"},
				repos.ReadTarget{RI: repoB, Branch: "agent/test"})
			require.True(t, b.IsLens())
			p := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
			ctx := WithEndpoint(auth.WithRepoReach(auth.WithPrincipal(repos.WithBinding(context.Background(), b), p), auth.AllRepos), "/api/v1/lenses/x/mcp")
			meter.Store(0)

			handler := MaterializeHandler(nil)
			if tc.hoist {
				handler = MaterializeHandler(nil, emb)
			}
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{"search": map[string]any{"text": "alpha"}, "depth": 0}
			res, err := handler(ctx, req)
			require.NoError(t, err)
			text := resultText(t, res)
			require.False(t, res.IsError, text)
			var page materialize.FactSetPage
			require.NoError(t, json.Unmarshal([]byte(text), &page))
			require.Equal(t, 2, page.Units, "both mounts' facts")
			require.Equal(t, tc.want, meter.Load(), "EmbedQuery calls across 2 mounts")
		})
	}
}

// Cross-endpoint (review L3): a multi-member lens's v2 cursor on the write
// repo's own endpoint, and that repo's cursor on the lens endpoint, both get
// exactly the text an unknown cursor gets on that endpoint (the pins differ:
// lens:<uid> against repo:<uid>), and nothing is served.
//
// The pin and the read-set fingerprint each refuse it on their own (dropping
// one alone is not caught here, by design: they are redundant).
//
// Sabotage (drop BOTH the pin and the read-set comparisons in
// IsFactSetCursor and openFactSet): the repo cursor is resumed on the lens
// endpoint and answers a different text.
func TestFactSet_Lens_CursorsDoNotCrossEndpoints(t *testing.T) {
	m, alpha, beta, _, _, lens := newLensE2E(t)
	obs(t, alpha, "kb/architecture/u/a1.md", "A1")
	obs(t, beta, "kb/architecture/u/b1.md", "B1")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	lensCtx, repoCtx := lensCtxFor(t, m, laptop, lens), repoCtxFor(alpha, laptop)
	first := func(ctx context.Context) materialize.FactSetPage {
		res, text := matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{"path": "kb/architecture/u/"}, "depth": 0, "limit": 1})
		require.False(t, res.IsError, text)
		var p materialize.FactSetPage
		require.NoError(t, json.Unmarshal([]byte(text), &p))
		require.NotNil(t, p.Cursor)
		return p
	}
	lp, rp := first(lensCtx), first(repoCtx)
	require.Equal(t, 2, lp.Units, "the lens FactSet (v2) spans both members")
	require.Equal(t, 1, rp.Units)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	call := func(ctx context.Context, cursor string) (string, bool) {
		res, text := matCallRaw(t, ctx, m, map[string]any{"cursor": cursor})
		return text, res.IsError
	}
	ghostRepo, isErr := call(repoCtx, ghost)
	require.True(t, isErr)
	got, isErr := call(repoCtx, *lp.Cursor)
	require.True(t, isErr, "the lens cursor must not page on the repo endpoint: %s", got)
	require.Equal(t, ghostRepo, got, "refused exactly like an unknown cursor")
	ghostLens, isErr := call(lensCtx, ghost)
	require.True(t, isErr)
	got, isErr = call(lensCtx, *rp.Cursor)
	require.True(t, isErr, "the repo cursor must not page on the lens endpoint: %s", got)
	require.Equal(t, ghostLens, got)
}
