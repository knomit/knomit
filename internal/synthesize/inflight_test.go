package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The in-flight refresh tests drive reviewStrategy.Apply — the production call
// site — rather than refreshInFlightItems directly. A test that calls the
// helper asserts the helper works, not that anything calls it; S5 in the
// commit-1 table is the same lesson from the other side.

// inflightEnv is a corpus with a duplicate pair plus enough motif-less filler
// that the backfill budget can be observed refilling.
type inflightEnv struct {
	*restatementEnv
	sess *store.PipelineSession
}

const (
	dupAPath   = "kb/technology/security/vulnerabilities/network-security/dup-a.md"
	dupBPath   = "kb/technology/security/vulnerabilities/network-appliances/dup-b.md"
	mergedPath = "kb/technology/security/vulnerabilities/network-security/merged.md"
)

func newInflightEnv(t *testing.T) *inflightEnv {
	t.Helper()
	// maxBackfillFacts + 4 filler facts, so the payload is over-subscribed and
	// a freed slot has a real candidate to go to. Asserted below rather than
	// assumed.
	env := newRestatementEnv(t, maxBackfillFacts+4)
	env.writeFact(dupAPath, "SmartConsole bypass", "one recording of the event")
	env.writeFact(dupBPath, "SmartConsole bypass, again", "the same event, second category")
	sess, err := env.svc.Pipeline().CreatePipelineSession(context.Background(), reviewTool, env.branch)
	require.NoError(t, err)
	return &inflightEnv{restatementEnv: env, sess: sess}
}

// queue inserts a work item exactly as Plan would, and returns its id.
func (e *inflightEnv) queue(stepType, clusterKey string, payload any, priority float64) int64 {
	e.t.Helper()
	blob, err := json.Marshal(payload)
	require.NoError(e.t, err)
	require.NoError(e.t, e.svc.Pipeline().InsertPipelineWorkItem(context.Background(), store.PipelineWorkItem{
		SessionID: e.sess.ID, StepType: stepType, ClusterKey: clusterKey,
		FactsJSON: string(blob), Priority: priority,
	}))
	// The highest id is the row just inserted. Deliberately NOT "the last item
	// in the pending list": that list is in QUEUE order (priority DESC, id
	// ASC), so the newest row is rarely last.
	var newest int64
	for _, it := range e.pending() {
		if it.ID > newest {
			newest = it.ID
		}
	}
	return newest
}

func (e *inflightEnv) pending() []store.PipelineWorkItem {
	e.t.Helper()
	items, err := e.svc.Pipeline().PendingPipelineWorkItems(context.Background(), e.sess.ID)
	require.NoError(e.t, err)
	return items
}

func (e *inflightEnv) itemByID(id int64) *store.PipelineWorkItem {
	e.t.Helper()
	for _, it := range e.pending() {
		if it.ID == id {
			return &it
		}
	}
	return nil
}

func members(paths ...string) []factForLLM {
	out := make([]factForLLM, 0, len(paths))
	for _, p := range paths {
		out = append(out, factForLLM{File: p, Title: p, Body: "b", Type: "observation"})
	}
	return out
}

// mergeResponse is the judge's answer merging dup-a and dup-b into one fact.
func mergeResponse() string {
	return fmt.Sprintf(`{"decisions":[],"merges":[{"paths":[%q,%q],"merged":{
		"path":%q,"title":"SmartConsole bypass","body":"Both recordings, unioned.",
		"type":"observation","domain":["security"],"confidence":0.9,"sources":2,
		"entities":["SmartConsole"],"refs":[]}}]}`, dupAPath, dupBPath, mergedPath)
}

// applyMerge answers the given prune item with the merge, through Decode+Apply.
func (e *inflightEnv) applyMerge(itemID int64) {
	e.t.Helper()
	ctx := context.Background()
	item := e.itemByID(itemID)
	require.NotNil(e.t, item, "the item to answer must still be queued")
	dec, _, err := reviewStrategy{}.Decode(item, mergeResponse())
	require.NoError(e.t, err)
	claimed, err := e.svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, mergeResponse())
	require.NoError(e.t, err)
	require.True(e.t, claimed)
	require.NoError(e.t, reviewStrategy{}.Apply(ctx, e.deps(), e.sess, item, dec))
	// The merge must actually have happened, or every assertion below is about
	// a session in which nothing was retired.
	gone, err := e.svc.Facts().FactExists(ctx, e.branch, dupBPath)
	require.NoError(e.t, err)
	require.False(e.t, gone, "the merge must have retired the loser, or this fixture proves nothing")
}

// TestInFlightRefresh_MergeFreesTheBackfillBudget is #127's (a): a confirmed
// duplicate must not go on costing one of the session's backfill slots.
//
// The backfill payload is materialised during Plan, so a merge applied later in
// the SAME session cannot reach it — the retired fact keeps its slot and the
// session offers seven live facts where it budgeted eight.
func TestInFlightRefresh_MergeFreesTheBackfillBudget(t *testing.T) {
	ctx := context.Background()
	env := newInflightEnv(t)

	// The payload as Plan would build it, then forced to offer the duplicate —
	// which is what Plan does anyway when the duplicate is among the oldest
	// motif-less facts.
	planned, err := backfillPayloadFor(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	require.Len(t, planned.Facts, maxBackfillFacts,
		"fixture must over-subscribe the budget, or a freed slot has nowhere to go")
	planned.Facts[0] = backfillItem{Path: dupBPath, FactID: env.liveFactIDs()[dupBPath]}
	backfillID := env.queue(motifBackfillStepType, "motif-backfill", planned, motifBackfillPriority)

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	env.applyMerge(pruneID)

	refreshed := env.itemByID(backfillID)
	require.NotNil(t, refreshed, "the backfill item must survive — it still has live facts to offer")
	var got backfillPayload
	require.NoError(t, json.Unmarshal([]byte(refreshed.FactsJSON), &got))

	for _, f := range got.Facts {
		require.NotEqual(t, dupBPath, f.Path, "a retired fact must not still be offered for backfill")
		require.NotEqual(t, dupAPath, f.Path, "a retired fact must not still be offered for backfill")
	}
	require.Len(t, got.Facts, maxBackfillFacts,
		"the freed slot must go to a live fact — filtering alone would leave the session "+
			"offering fewer facts than its budget")
}

// TestInFlightRefresh_QueuedPruneItemLosesTheMergedFact — a still-queued
// consolidation item stops naming a fact the session has already retired,
// while keeping the members it can still judge.
func TestInFlightRefresh_QueuedPruneItemLosesTheMergedFact(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	env.writeFact("kb/other-y.md", "Y", "y")

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	queuedID := env.queue("prune", "cluster-1", members(dupBPath, "kb/other-x.md", "kb/other-y.md"), 3)

	env.applyMerge(pruneID)

	still := env.itemByID(queuedID)
	require.NotNil(t, still, "two live members remain — the item is still judgeable")
	var facts []factForLLM
	require.NoError(t, json.Unmarshal([]byte(still.FactsJSON), &facts))
	require.Len(t, facts, 2)
	for _, f := range facts {
		require.NotEqual(t, dupBPath, f.File, "a retired fact must not be re-offered to the judge")
	}
}

// TestInFlightRefresh_ItemBelowTheJudgeableFloorIsDropped pins the FLOOR, not
// merely emptiness: one surviving member is not a consolidation question, and
// an item reduced to one would spend a judge slot asking whether a single fact
// should be merged with itself.
func TestInFlightRefresh_ItemBelowTheJudgeableFloorIsDropped(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	queuedID := env.queue("prune", "cluster-1", members(dupBPath, "kb/other-x.md"), 3)

	env.applyMerge(pruneID)

	require.Nil(t, env.itemByID(queuedID),
		"an item left with fewer than two members has nothing to judge and must leave the queue")
}

// TestInFlightRefresh_UntouchedItemsKeepTheirExactPayload is the guard against
// the opposite failure: a refresh that re-rolled every queued item on every
// merge would churn payloads the agent may already be holding, and would make
// the backfill item a different offer each time anything merged anywhere.
func TestInFlightRefresh_UntouchedItemsKeepTheirExactPayload(t *testing.T) {
	ctx := context.Background()
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	env.writeFact("kb/other-y.md", "Y", "y")

	planned, err := backfillPayloadFor(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	// Strip both duplicates from the offer so this payload names nothing that
	// the merge below retires, and then cut it SHORT.
	//
	// The short cut is what makes this test discriminate. A payload equal to
	// what a re-derivation would produce cannot tell "left alone" from
	// "re-derived and coincidentally identical" — the first draft of this test
	// had exactly that hole and passed under the sabotage it exists to catch.
	var clean []backfillItem
	for _, f := range planned.Facts {
		if f.Path != dupAPath && f.Path != dupBPath {
			clean = append(clean, f)
		}
	}
	require.Greater(t, len(clean), 3, "fixture needs room to be cut short")
	planned.Facts = clean[:3]
	backfillID := env.queue(motifBackfillStepType, "motif-backfill", planned, motifBackfillPriority)

	// Assert the fixture is DISTINGUISHABLE: a re-derivation right now would
	// return a different offer, so an unchanged payload below is evidence.
	wouldBe, err := backfillPayloadFor(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	require.NotEqual(t, len(planned.Facts), len(wouldBe.Facts),
		"a re-derived payload must differ from the queued one, or this test cannot fail")
	untouchedID := env.queue("prune", "cluster-1", members("kb/other-x.md", "kb/other-y.md"), 3)

	beforeBackfill := env.itemByID(backfillID).FactsJSON
	beforeUntouched := env.itemByID(untouchedID).FactsJSON

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	env.applyMerge(pruneID)

	require.Equal(t, beforeBackfill, env.itemByID(backfillID).FactsJSON,
		"a backfill payload naming no retired fact must not be re-derived")
	require.Equal(t, beforeUntouched, env.itemByID(untouchedID).FactsJSON,
		"an item naming no retired fact must be left exactly as planned")
}

// failingDelete wraps a FactIndex and refuses to delete one path, so a test can
// drive the case where the corpus did NOT actually lose the fact.
type failingDelete struct {
	store.FactIndex
	refuse string
}

func (f failingDelete) DeleteFact(ctx context.Context, branch, path, message string) (string, error) {
	if path == f.refuse {
		return "", fmt.Errorf("simulated delete failure for %s", path)
	}
	return f.FactIndex.DeleteFact(ctx, branch, path, message)
}

// TestApplyPrune_RetiredNamesOnlyFactsThatActuallyLeft — ReviewStats.Retired
// drives a refresh that STRIPS facts from queued items, so a path reported as
// retired when its delete failed would remove a live fact from the judge's
// view. The retract branch used to mark the path deleted before attempting the
// delete, which made a failed retract indistinguishable from a completed one.
func TestApplyPrune_RetiredNamesOnlyFactsThatActuallyLeft(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/keep-me.md", "Keeps", "survives a failed delete")
	env.writeFact("kb/goes.md", "Goes", "deleted for real")

	d := env.deps()
	gs := failingDelete{FactIndex: env.svc.Facts(), refuse: "kb/keep-me.md"}
	stats, err := ApplyPruneDecisions(ctx, gs, env.svc.Search(),
		[]PruneDecision{{Path: "kb/keep-me.md", Action: "retract"}, {Path: "kb/goes.md", Action: "retract"}},
		nil, reviewTool, d.OnProgress, env.branch, "", "kb")
	require.NoError(t, err)

	require.NotContains(t, stats.Retired, "kb/keep-me.md",
		"a fact whose delete FAILED is still live and must not be reported as retired")
	require.Contains(t, stats.Retired, "kb/goes.md")

	exists, err := env.svc.Facts().FactExists(ctx, env.branch, "kb/keep-me.md")
	require.NoError(t, err)
	require.True(t, exists, "the fixture must leave the refused fact live, or this test proves nothing")
}
