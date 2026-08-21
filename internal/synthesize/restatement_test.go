package synthesize

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEnsureTitleVectors_WatermarkIncremental is the conformance test for
// "a second review session on an unchanged corpus embeds ZERO titles, and a
// session after one fact edit embeds exactly one".
//
// Both are measured AFTER coverage completes, because a time-budgeted backfill
// legitimately continues across sessions until then — see
// TestEnsureTitleVectors_StopsAtTheLatencyBudget for that half.
func TestEnsureTitleVectors_WatermarkIncremental(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 3)

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, 3, total, "coverage completes in one session at this size")
	require.Equal(t, int64(3), env.emb.titles.Load())

	env.emb.titles.Store(0)
	have, _, err = ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, int64(0), env.emb.titles.Load(), "unchanged corpus embeds ZERO titles")

	env.writeFact("kb/f1.md", "F1 revised", "body-1-v2")

	env.emb.titles.Store(0)
	have, total, err = ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, 3, total)
	require.Equal(t, int64(1), env.emb.titles.Load(), "one edited fact embeds exactly one title")
}

// TestEnsureTitleVectors_UsesTheShortStringTemplate — MN9's guard on this side
// of the seam. Titles are short strings; embedding them as documents would put
// them through a rendering nothing in the motif work was calibrated under.
func TestEnsureTitleVectors_UsesTheShortStringTemplate(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 2)

	// Writing the facts embeds their bodies through the document path, which is
	// the write path's business — snapshot that counter so this test speaks
	// only about what the BACKFILL does.
	docsBefore := env.emb.documentCalls.Load()

	_, _, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)

	require.Positive(t, env.emb.shortStringCalls.Load(), "titles embed as short strings")
	require.Equal(t, docsBefore, env.emb.documentCalls.Load(),
		"the backfill never embeds a title through the document path")
}

// TestEnsureTitleVectors_StopsAtTheLatencyBudget proves the budget is honoured
// and that partial coverage is REPORTED rather than hidden — a silently partial
// axis reads as "nothing to find".
func TestEnsureTitleVectors_StopsAtTheLatencyBudget(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 200)
	env.emb.perBatchDelay = 20 * time.Millisecond

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, 50*time.Millisecond)
	require.NoError(t, err)
	require.Greater(t, have, 0, "it makes progress")
	require.Less(t, have, total, "and stops before coverage completes")

	// Coverage completes over subsequent sessions rather than being abandoned.
	for range 20 {
		have, total, err = ensureTitleVectors(ctx, env.deps(), env.branch, 50*time.Millisecond)
		require.NoError(t, err)
		if have == total {
			break
		}
	}
	require.Equal(t, total, have, "later sessions finish the backfill")
}

// TestEnsureTitleVectors_NoEmbedderIsANoOp — read-only tooling and tests run
// without an embedder; the axis stays empty and nothing errors.
func TestEnsureTitleVectors_NoEmbedderIsANoOp(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnvWithoutEmbedder(t, 2)

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Zero(t, have)
	require.Zero(t, total)
}
