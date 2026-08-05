package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Tests for the session summary. ReviewResult.Summary was hardcoded to an
// empty &ReviewStats{} at completion, so every session reported that it had
// changed nothing — even though ApplyPruneDecisions/ApplyDistillDecisions
// return real counts. The counts now accumulate on the pipeline_sessions row
// (they cannot live on the Reviewer: the MCP handler builds a fresh one per
// call, see invariants/synthesize/per-call-objects-no-session-state) and the
// completing call reads them back.

// TestReviewer_Summary_CountsPruneAndDistill is the acceptance test: a session
// that retracts one fact and synthesizes one fact must report exactly that.
//
// The two applies happen in separate ContinueSession calls, which is the point
// — a per-call counter would report only the last item's work.
func TestReviewer_Summary_CountsPruneAndDistill(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	// The retract target must really exist on the branch: DeleteFact failures
	// degrade to a warn and would leave Pruned at 0 for the wrong reason.
	//
	// It is deliberately the SAME path distillResponseOneFact cites. Item 1
	// retracts it, item 2 then synthesizes a fact citing it — which is the real
	// shape of a review session, and must work: a retracted fact stays reachable
	// by walk-back, so the citation resolves exactly as the reader resolves it.
	const prunePath = "kb/technology/a.md"
	seedObservation(t, svc, branch, prunePath)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, prunePath)
	insertManualDistillItem(t, svc, sess.ID)

	// Item 1: prune, retracting the seeded fact.
	res, err := r.ContinueSession(ctx, sess.ID, `{
		"decisions": [{"path": "`+prunePath+`", "action": "retract"}],
		"merges": []
	}`)
	require.NoError(t, err)
	require.False(t, res.Done, "the distill item must still be pending")

	// Item 2: distill, synthesizing one fact. No watermark is set, so there are
	// no hypothesis transitions and the session completes on this call.
	res, err = r.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.True(t, res.Done, "session must complete once both items are answered")
	require.NotNil(t, res.Summary, "a completed session must carry a summary")

	require.Equal(t, ReviewStats{Pruned: 1, Synthesized: 1}, *res.Summary,
		"summary must report the work both items actually did")
}

// TestReviewer_Summary_ZeroWhenNothingChanged pins the other direction: a
// no-op session reports zeros rather than inheriting counts from anywhere
// else. This is also the pre-existing wire shape, so it doubles as the
// compatibility check — only the numbers changed, not the field.
func TestReviewer_Summary_ZeroWhenNothingChanged(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	sess := manualSession(t, svc, "agent/test")
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotNil(t, res.Summary)
	require.Equal(t, ReviewStats{}, *res.Summary, "a no-op session must report zeros")
}

// TestReviewer_Summary_SurvivesFreshReviewer is the regression anchor for
// where the totals live. The two items are answered through two different
// Reviewer instances, mirroring the MCP handler's per-call construction. Any
// implementation that accumulated on the struct would report only the second
// item's work here.
func TestReviewer_Summary_SurvivesFreshReviewer(t *testing.T) {
	r1, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	const prunePath = "kb/technology/a.md"
	seedObservation(t, svc, branch, prunePath)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, prunePath)
	insertManualDistillItem(t, svc, sess.ID)

	_, err := r1.ContinueSession(ctx, sess.ID, `{
		"decisions": [{"path": "`+prunePath+`", "action": "retract"}],
		"merges": []
	}`)
	require.NoError(t, err)

	r2 := NewReviewer(r1.ri, nil)
	res, err := r2.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotNil(t, res.Summary)
	require.Equal(t, 1, res.Summary.Pruned,
		"the first Reviewer's prune must still be counted — totals live on the session row")
	require.Equal(t, 1, res.Summary.Synthesized)
}

// seedObservation writes a minimal observation fact so that decisions naming
// it (retract, update) operate on a path that genuinely exists in git.
func seedObservation(t *testing.T, svc *store.Service, branch, path string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "A"
	f.Body = "seeded observation"
	f.Type = fact.Observation
	f.Confidence = 0.7
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, path, body, "seed-observation", "")
	require.NoError(t, err)
}
