package synthesize

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Tests for the P0.4 claim protocol in ContinueSession: peek → decode+validate
// → CAS-claim → apply.
//
// Pre-fix the order was peek → decode → apply → mark-answered, so the window
// between "facts written" and "item recorded as answered" was wide open: two
// callers racing on the same item both saw it unanswered and both applied,
// and a caller whose mark-answered failed left the item to be re-served and
// re-applied. Because ApplyDistillDecisions mints a fresh UUID filename per
// call (decision.go normalizeFactPath), each extra apply is a *new* fact, not
// an overwrite — duplicate synthesis facts, i.e. corpus corruption.

// distillResponseOneFact is a valid distill response that synthesizes exactly
// one fact and retracts nothing. Applying it twice yields two facts on disk;
// applying it once yields one. That count is the whole assertion.
const distillResponseOneFact = `{
	"synthesize": [{
		"path": "kb/technology/synth.md",
		"title": "S",
		"body": "distilled body",
		"type": "synthesis",
		"domain": ["technology"],
		"confidence": 0.8,
		"entities": [],
		"refs": ["kb/technology/a.md"]
	}],
	"retract": []
}`

// TestReviewer_DuplicateDistillSubmission_SynthesizesOnce is the P0.4
// regression anchor: the same distill response submitted twice against the
// same work item must produce exactly one set of synthesized facts.
//
// The two submissions are concurrent because that is the shape in which the
// bug is reachable: a sequential resubmission after a *successful* call already
// found the item answered even pre-fix. What was unguarded is any pair of
// applies that both peeked before either recorded its answer — a racing client,
// a retried MCP call, a duplicated tool invocation. Post-fix the claim CAS runs
// before apply, so exactly one submission may write.
func TestReviewer_DuplicateDistillSubmission_SynthesizesOnce(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	itemID := insertManualDistillItem(t, svc, sess.ID)

	errs := submitConcurrently(t, r, sess.ID, distillResponseOneFact, 2)
	for i, err := range errs {
		require.NoErrorf(t, err, "submission %d errored", i)
	}

	require.Len(t, synthesisFacts(t, svc, branch), 1,
		"the same distill response submitted twice must synthesize exactly one fact")

	// And the item is consumed exactly once — a second claim on it is a no-op.
	claimed, err := svc.Pipeline().AnswerPipelineWorkItem(ctx, itemID, "again")
	require.NoError(t, err)
	require.False(t, claimed, "the distill item must already be claimed")
}

// TestReviewer_ConcurrentContinuations_ApplyOnce widens the race to four
// callers and pins the other half of the CAS contract: losing the claim is
// benign. A loser must return normally (dispatching to whatever is next), not
// surface an error to the agent — the item genuinely was handled, just not by
// this caller.
func TestReviewer_ConcurrentContinuations_ApplyOnce(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	insertManualDistillItem(t, svc, sess.ID)

	errs := submitConcurrently(t, r, sess.ID, distillResponseOneFact, 4)
	for i, err := range errs {
		require.NoErrorf(t, err, "caller %d errored; a lost claim must be benign", i)
	}

	require.Len(t, synthesisFacts(t, svc, branch), 1,
		"four concurrent continuations of one item must apply exactly once")
}

// TestReviewer_MalformedDistillResponse_LeavesItemRetryable pins the reason
// decode runs *before* the claim: malformed LLM JSON is the common failure
// class, and burning the item on it would silently drop that item's work.
// The item must survive unclaimed and still accept a corrected response.
func TestReviewer_MalformedDistillResponse_LeavesItemRetryable(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	itemID := insertManualDistillItem(t, svc, sess.ID)

	_, err := r.ContinueSession(ctx, sess.ID, `{"synthesize": [ this is not json`)
	require.Error(t, err, "a malformed distill response must be rejected")
	require.Empty(t, synthesisFacts(t, svc, branch), "a rejected response must write nothing")

	// Unclaimed: the same item is still the one served.
	next, err := svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, next, "a rejected response must leave the item unanswered")
	require.Equal(t, itemID, next.ID)

	// Retryable: the corrected response applies normally.
	_, err = r.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.Len(t, synthesisFacts(t, svc, branch), 1,
		"the retried response must apply exactly once")
}

// TestReviewer_StaleItemID_Rejected covers D2. A client that answers an item
// which is no longer current must be refused rather than have its response
// applied to a different item — the response was reasoned about against the
// stale item's facts, so it would be validated against the wrong input paths.
// The refusal must not consume the current item either: it is still answerable.
func TestReviewer_StaleItemID_Rejected(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	itemID := insertManualDistillItem(t, svc, sess.ID)

	_, err := r.ContinueSessionForItem(ctx, sess.ID, distillResponseOneFact, itemID+999)
	require.Error(t, err, "a stale item_id must be rejected")
	require.Contains(t, err.Error(), "is current")
	require.Empty(t, synthesisFacts(t, svc, branch), "a stale answer must write nothing")

	next, err := svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, next, "a rejected stale answer must not consume the current item")
	require.Equal(t, itemID, next.ID)

	// The matching id is accepted, proving the guard is an equality check and
	// not a blanket rejection of the argument.
	_, err = r.ContinueSessionForItem(ctx, sess.ID, distillResponseOneFact, itemID)
	require.NoError(t, err)
	require.Len(t, synthesisFacts(t, svc, branch), 1)
}

// TestReviewer_RenderedItemCarriesID asserts the D2 wire field is populated:
// a client cannot echo back an id it was never given.
func TestReviewer_RenderedItemCarriesID(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	sess := manualSession(t, svc, "agent/test")
	itemID := insertManualDistillItem(t, svc, sess.ID)

	res, err := r.nextItem(ctx, sess)
	require.NoError(t, err)
	require.NotNil(t, res.Item)
	require.Equal(t, itemID, res.Item.ID, "the rendered item must carry its work-item id")
}

// submitConcurrently fires n ContinueSession calls at the same session from n
// goroutines, released together so their peeks interleave. Returns each
// caller's error in call order.
func submitConcurrently(t *testing.T, r *Reviewer, sessionID, response string, n int) []error {
	t.Helper()
	errs := make([]error, n)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			start.Wait()
			_, errs[i] = r.ContinueSession(context.Background(), sessionID, response)
		}()
	}
	start.Done()
	done.Wait()
	return errs
}

// insertManualDistillItem queues a single distill work item over two synthetic
// input paths and returns its id.
//
// Depth is pinned at maxRaptorDepth so applying the item enqueues no RAPTOR
// follow-up distill items. These tests assert on what a *specific* item's
// apply produced, and a follow-up item landing in the queue mid-test would
// change which item a subsequent peek returns.
func insertManualDistillItem(t *testing.T, svc *store.Service, sessionID string) int64 {
	t.Helper()
	ctx := context.Background()
	facts := []factForLLM{
		{File: "kb/technology/a.md", Title: "A", Body: "a", Type: "observation"},
		{File: "kb/technology/b.md", Title: "B", Body: "b", Type: "observation"},
	}
	body, err := json.Marshal(facts)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sessionID,
		StepType:   "distill",
		ClusterKey: "distill-all",
		FactsJSON:  string(body),
		Priority:   0,
		Depth:      maxRaptorDepth,
	}))
	item, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, item)
	return item.ID
}

// synthesisFacts returns every synthesis fact live on the branch. Distill
// writes each synthesized fact under a freshly minted UUID filename, so a
// duplicated apply shows up as an extra element here rather than as an
// overwritten one.
func synthesisFacts(t *testing.T, svc *store.Service, branch string) []store.SearchResult {
	t.Helper()
	results, err := svc.Search().Search(context.Background(), branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        1000,
	})
	require.NoError(t, err)
	return results
}
