package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestBackwardDiscoverPriority_StrictlyNegativeRanked is the regression guard
// for the backward priority-leak bug, mirroring the forward path's
// TestForwardDiscoverPriority_StrictlyNegativeRanked.
//
// Backward "discover" work items must run AFTER the per-fact hypothesize items,
// which carry positive priorities, so their priority must stay strictly negative
// regardless of how large a member's BlastRadius is. The old formula fed blast
// straight into the priority (`-100 + blast`); a high-blast keystone then
// produced a positive priority that leapfrogged the per-fact band. The fix ranks
// by position in the BlastRadius-sorted slice, so priority is a function of rank
// only — backwardDiscoverPriority takes no blast argument at all, so blast
// magnitude cannot leak in by construction.
func TestBackwardDiscoverPriority_StrictlyNegativeRanked(t *testing.T) {
	// Even a very large discover queue stays strictly below the per-fact band (0).
	prev := 0.0
	for rank := 0; rank < 1000; rank++ {
		p := backwardDiscoverPriority(rank)
		if p >= 0 {
			t.Fatalf("backwardDiscoverPriority(%d) = %v, must be strictly negative (below the per-fact band)", rank, p)
		}
		if rank > 0 && p >= prev {
			t.Fatalf("priority must strictly decrease with rank: rank %d = %v, prev = %v", rank, p, prev)
		}
		prev = p
	}

	// Highest-blast keystone (rank 0) sits at the top of the discover band.
	if got := backwardDiscoverPriority(0); got != backwardDiscoverPriorityBase {
		t.Errorf("rank 0 priority = %v, want %v (top of discover band)", got, backwardDiscoverPriorityBase)
	}

	// The base must be strictly negative so discover never outranks the
	// positive-priority per-fact hypothesize items.
	if backwardDiscoverPriorityBase >= 0 {
		t.Errorf("discover band base %v must be strictly negative", backwardDiscoverPriorityBase)
	}
}

// TestHypothesizeContinue_DiscoverParseFailure_NonFatal mirrors the forward
// path's TestReviewer_DiscoverParseFailure_NonFatal: a malformed discover
// *response* (the model's answer) must be non-fatal — the work item is still
// marked answered, no fact is written, and hypothesizeContinue returns no error
// rather than aborting the session.
func TestHypothesizeContinue_DiscoverParseFailure_NonFatal(t *testing.T) {
	_, ri, realS := openHypothesizeTestStore(t)
	ctx := context.Background()
	branch := "agent/test"

	sess, err := realS.pipeline.CreatePipelineSession(ctx, "hypothesize", branch)
	require.NoError(t, err)

	// A well-formed discover work item (forward direction avoids the
	// backward-only blast gate; irrelevant here since apply never runs).
	payloadJSON := `{"direction":"forward","bridge":{"token":"auth","kind":"entity","members":[]}}`
	require.NoError(t, realS.pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "discover",
		ClusterKey: "discover-bwd-0",
		FactsJSON:  payloadJSON,
		Priority:   backwardDiscoverPriority(0),
	}))

	// Malformed response (not the work-item payload): the model returned garbage.
	_, err = hypothesizeContinue(ctx, ri, realS, branch, sess.ID, `not json {{{`)
	require.NoError(t, err, "a malformed discover response must be non-fatal")

	// No fact may be written from an unparseable response.
	results, searchErr := realS.search.Search(ctx, branch, store.SearchOptions{Limit: 10})
	require.NoError(t, searchErr)
	require.Empty(t, results, "no fact must be written when the discover response is unparseable")

	// The work item must still have been marked answered (no retry loop).
	next, nerr := realS.pipeline.NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, nerr)
	require.Nil(t, next, "the discover item must be marked answered, leaving no unanswered work")
}
