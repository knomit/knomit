package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests for the work-item claim CAS (AnswerPipelineWorkItem) and the
// deterministic peek order it depends on.
//
// The pre-fix code answered an item with an unconditional UPDATE, so a
// resubmitted response looked indistinguishable from a first submission and
// the caller re-applied its mutations — minting a second copy of every fact
// the response synthesized. The CAS makes "may I apply this?" a question the
// database answers exactly once per item.

// TestAnswerPipelineWorkItem_ClaimsExactlyOnce is the core contract: the first
// answer wins the claim, every subsequent answer for the same item is a benign
// no-op rather than an error, and the stored response is the winner's.
func TestAnswerPipelineWorkItem_ClaimsExactlyOnce(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)
	require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: "c0", FactsJSON: "[]",
	}))

	item, err := pi.NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, item)

	claimed, err := pi.AnswerPipelineWorkItem(ctx, item.ID, "first")
	require.NoError(t, err)
	require.True(t, claimed, "the first answer must win the claim")

	claimed, err = pi.AnswerPipelineWorkItem(ctx, item.ID, "second")
	require.NoError(t, err, "a lost claim must be benign, not an error")
	require.False(t, claimed, "an already-answered item must not be claimable again")

	// The item leaves the queue, and the winner's response is what persisted:
	// a losing caller must not be able to overwrite the applied response.
	next, err := pi.NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, next, "an answered item must no longer be served")

	var stored string
	require.NoError(t, svc.Pipeline().(*pipelineIndex).sessionDB.
		QueryRowContext(ctx, `SELECT response FROM pipeline_work_items WHERE id = ?`, item.ID).
		Scan(&stored))
	require.Equal(t, "first", stored, "the claim winner's response must be the one stored")
}

// TestAnswerPipelineWorkItem_UnknownID reports no claim for an id that does
// not exist, rather than erroring — a missing row is indistinguishable from an
// already-answered one from the caller's perspective, and both mean "do not
// apply".
func TestAnswerPipelineWorkItem_UnknownID(t *testing.T) {
	svc := newPhaseTestService(t)
	claimed, err := svc.Pipeline().AnswerPipelineWorkItem(context.Background(), 987654, "x")
	require.NoError(t, err)
	require.False(t, claimed)
}

// TestNextPipelineWorkItem_TiebreakByIDAscending pins the `priority DESC,
// id ASC` ordering. Priority alone does not totally order the queue — every
// top-level distill item carries priority 0.0 — so without the id tiebreak
// SQLite may return ties in any order and two peeks of the same queue state
// can hand back different items. Insertion order must win ties.
func TestNextPipelineWorkItem_TiebreakByIDAscending(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)

	// Three items at the same priority, plus one strictly higher that must
	// still be served first (the tiebreak may not override priority).
	for _, key := range []string{"tie-a", "tie-b", "tie-c"} {
		require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
			SessionID: sess.ID, StepType: "distill", ClusterKey: key, FactsJSON: "[]", Priority: 0,
		}))
	}
	require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: "high", FactsJSON: "[]", Priority: 5,
	}))

	want := []string{"high", "tie-a", "tie-b", "tie-c"}
	for _, expect := range want {
		item, err := pi.NextPipelineWorkItem(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, item, "expected item %q, got empty queue", expect)
		require.Equal(t, expect, item.ClusterKey,
			"queue must drain by priority DESC then insertion order")
		claimed, err := pi.AnswerPipelineWorkItem(ctx, item.ID, "ok")
		require.NoError(t, err)
		require.True(t, claimed)
	}

	// Repeated peeks of an unchanged queue must be stable, not merely
	// eventually-correct: that stability is what makes the item_id echo
	// meaningful to a client.
	require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
		SessionID: sess.ID, StepType: "distill", ClusterKey: "stable-1", FactsJSON: "[]", Priority: 0,
	}))
	require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
		SessionID: sess.ID, StepType: "distill", ClusterKey: "stable-2", FactsJSON: "[]", Priority: 0,
	}))
	first, err := pi.NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	for range 5 {
		again, err := pi.NextPipelineWorkItem(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, first.ID, again.ID, "peek must be a deterministic function of queue state")
	}
}
