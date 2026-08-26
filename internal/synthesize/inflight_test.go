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
	// Filler facts so the queued items have a corpus around them. The size was
	// once the backfill budget (8) plus 4, chosen to over-subscribe the
	// backfill payload so a freed slot had a real candidate; backfill is gone
	// and nothing here is budget-sensitive any more. Kept at the same COUNT
	// deliberately, so this removal does not quietly change the fixture the
	// surviving refresh tests run against.
	const fillerFacts = 12
	env := newRestatementEnv(t, fillerFacts)
	env.writeFact(dupAPath, "SmartConsole bypass", "one recording of the event")
	env.writeFact(dupBPath, "SmartConsole bypass, again", "the same event, second category")
	sess, err := env.svc.Pipeline().CreatePipelineSession(context.Background(), reviewTool, env.branch, "")
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
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	env.writeFact("kb/other-y.md", "Y", "y")

	// This test used to assert the same property on TWO vehicles: a queued
	// backfill payload and a queued prune item. The backfill half left with the
	// backfill pass; the property it checked is unchanged and the prune half
	// still checks it.
	//
	// It is the NEGATIVE control for
	// TestInFlightRefresh_QueuedPruneItemLosesTheMergedFact: that test proves an
	// item naming a retired fact IS rewritten, this one proves an item naming
	// none is left byte-identical. Neither discriminates alone — together they
	// pin that the refresh acts on exactly the items it should.
	untouchedID := env.queue("prune", "cluster-1", members("kb/other-x.md", "kb/other-y.md"), 3)
	beforeUntouched := env.itemByID(untouchedID).FactsJSON

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	env.applyMerge(pruneID)

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

// applyResponse answers a queued item through Decode+Apply, the production
// path, and asserts the claim was won.
func (e *inflightEnv) applyResponse(itemID int64, response string) {
	e.t.Helper()
	ctx := context.Background()
	item := e.itemByID(itemID)
	require.NotNil(e.t, item, "the item to answer must still be queued")
	dec, _, err := reviewStrategy{}.Decode(item, response)
	require.NoError(e.t, err)
	claimed, err := e.svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, response)
	require.NoError(e.t, err)
	require.True(e.t, claimed)
	require.NoError(e.t, reviewStrategy{}.Apply(ctx, e.deps(), e.sess, item, dec))
}

func (e *inflightEnv) factsOf(itemID int64) []factForLLM {
	e.t.Helper()
	item := e.itemByID(itemID)
	require.NotNil(e.t, item)
	var facts []factForLLM
	require.NoError(e.t, json.Unmarshal([]byte(item.FactsJSON), &facts))
	return facts
}

func confidenceOf(facts []factForLLM, path string) (float64, bool) {
	for _, f := range facts {
		if f.File == path {
			return f.Confidence, true
		}
	}
	return 0, false
}

// TestInFlightRefresh_UpdateRefreshesQueuedSnapshots — an item's payload is a
// SNAPSHOT of each fact's fields, so a confidence rewritten mid-session leaves
// every later item showing the old number. Measured live: facts set to
// 0.5/0.7/0.8 still read 0.9/0.9/0.95 at items queued before the update.
//
// Staleness here is not retirement — the fact is still live and still belongs
// in the item. The member has to be RE-READ, not dropped.
func TestInFlightRefresh_UpdateRefreshesQueuedSnapshots(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")

	judgedID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	queuedID := env.queue("prune", "cluster-1", members(dupAPath, "kb/other-x.md"), 3)

	before, ok := confidenceOf(env.factsOf(queuedID), dupAPath)
	require.True(t, ok)

	env.applyResponse(judgedID, fmt.Sprintf(
		`{"decisions":[{"path":%q,"action":"update","confidence":0.31}],"merges":[]}`, dupAPath))

	after, ok := confidenceOf(env.factsOf(queuedID), dupAPath)
	require.True(t, ok, "an UPDATED fact is still live and must stay in the item")
	require.NotEqual(t, before, after, "the queued snapshot must not keep the pre-update value")
	require.InDelta(t, 0.31, after, 0.001,
		"the queued item must show the confidence the corpus now holds")
}

// TestInFlightRefresh_ClearsAcrossVehicles — the queue a merge leaves stale is
// not its own. Measured live: a distill was offered over facts merged twenty
// minutes earlier, and 23 clusters this session reached prune AND distill.
func TestInFlightRefresh_ClearsAcrossVehicles(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	env.writeFact("kb/other-y.md", "Y", "y")

	distillID := env.queue("distill", "group-0",
		members(dupBPath, "kb/other-x.md", "kb/other-y.md"), 0)
	discoverID := env.queue("discover", "discover-fwd-0", DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token:   "SmartConsole",
			Members: members(dupBPath, "kb/other-x.md", "kb/other-y.md"),
		},
	}, -1)

	pruneID := env.queue("prune", "cluster-0", members(dupAPath, dupBPath), 2)
	env.applyMerge(pruneID)

	for _, f := range env.factsOf(distillID) {
		require.NotEqual(t, dupBPath, f.File,
			"a merged-away fact must not still be offered to distill")
	}

	item := env.itemByID(discoverID)
	require.NotNil(t, item)
	var payload DiscoverWorkPayload
	require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &payload))
	require.Len(t, payload.Bridge.Members, 2)
	for _, f := range payload.Bridge.Members {
		require.NotEqual(t, dupBPath, f.File,
			"a merged-away fact must not still count as a bridge member")
	}
}

// TestInFlightRefresh_DistillRetractClearsOtherQueues is the mirror: distill
// retires facts through its own retract list, and the items left holding them
// belong to other vehicles.
func TestInFlightRefresh_DistillRetractClearsOtherQueues(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	env.writeFact("kb/other-y.md", "Y", "y")

	queuedID := env.queue("prune", "cluster-1",
		members(dupBPath, "kb/other-x.md", "kb/other-y.md"), 3)
	distillID := env.queue("distill", "group-0", members(dupAPath, dupBPath), 0)

	env.applyResponse(distillID, fmt.Sprintf(
		`{"synthesize":[{"path":"kb/technology/security/vulnerabilities/network-security/distilled.md",
		"title":"One event","body":"Synthesized from both recordings.","type":"synthesis",
		"domain":["security"],"confidence":0.8,"entities":["SmartConsole"],"refs":[]}],
		"retract":[%q]}`, dupBPath))

	gone, err := env.svc.Facts().FactExists(context.Background(), env.branch, dupBPath)
	require.NoError(t, err)
	require.False(t, gone, "the distill must have retracted the fact, or this test proves nothing")

	still := env.itemByID(queuedID)
	require.NotNil(t, still)
	for _, f := range env.factsOf(queuedID) {
		require.NotEqual(t, dupBPath, f.File,
			"a fact distill retracted must not still be offered to prune")
	}
}

// TestApplyPrune_RewrittenExcludesPathsTheSameApplyRetired — Retired and
// Rewritten mean opposite things to a queued item: drop this member, versus
// re-read it. One judge response can do BOTH to a path (raise its confidence,
// then subsume it into a merge), and a path reported as rewritten when it is
// gone would send the refresh to re-read a fact the corpus no longer holds.
//
// The consumer happens to check `gone` first, so this contract is currently
// belt-and-braces. It is asserted here rather than left to that ordering: the
// two sets are read by more than one caller as the feature grows, and a
// contract that holds only because of the reader's statement order is not a
// contract.
func TestApplyPrune_RewrittenExcludesPathsTheSameApplyRetired(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact(dupAPath, "SmartConsole bypass", "one recording")
	env.writeFact(dupBPath, "SmartConsole bypass, again", "the same event")

	d := env.deps()
	var merged mergedFact
	require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{
		"path":%q,"title":"SmartConsole bypass","body":"Both recordings, unioned.",
		"type":"observation","domain":["security"],"confidence":0.9,"sources":2,
		"entities":["SmartConsole"],"refs":[]}`, mergedPath)), &merged))

	stats, err := ApplyPruneDecisions(ctx, env.svc.Facts(), env.svc.Search(),
		// Raise dup-a's confidence, then merge it away in the same response.
		[]PruneDecision{{Path: dupAPath, Action: "update", Confidence: 0.42}},
		[]MergeEntry{{Paths: []string{dupAPath, dupBPath}, Merged: merged}},
		reviewTool, d.OnProgress, env.branch, "", "kb")
	require.NoError(t, err)

	require.Contains(t, stats.Retired, dupAPath, "the fixture must actually retire the updated path")
	require.NotContains(t, stats.Rewritten, dupAPath,
		"a path this apply retired must not also be reported as rewritten — "+
			"there is nothing left to re-read")
}

// TestInFlightRefresh_ReinforcementRefreshesQueuedSnapshots — the third
// vehicle. Measured live this session: a merged pair came back in a DISTILL
// item and in a DISCOVER item, so all three work-item types re-serve stale
// facts.
//
// Discover has no retire path of its own — it WRITES new facts and REINFORCES
// existing ones, and nothing in applyDiscoverStep deletes anything. Its
// contribution to in-session staleness is therefore the mutated-but-live half:
// a reinforced fact gains a source and a ref, and every queued item is still
// carrying the count from before.
func TestInFlightRefresh_ReinforcementRefreshesQueuedSnapshots(t *testing.T) {
	env := newInflightEnv(t)
	env.writeFact("kb/other-x.md", "X", "x")
	// The reinforced fact is re-rendered through SerializeFact by the
	// reinforce path, which refuses any fact whose stored bytes would not
	// round-trip. Write it the way a real writer does, or the gate rejects it
	// and the test measures a rejection rather than a refresh.
	env.writeFactWithMotifsConf(dupAPath, "SmartConsole bypass", "one recording", nil, 0.7)

	queuedID := env.queue("prune", "cluster-1", members(dupAPath, "kb/other-x.md"), 3)
	before := sourcesOf(env.factsOf(queuedID), dupAPath)
	require.GreaterOrEqual(t, before, 0, "the queued item must carry the fact under test")

	discoverID := env.queue("discover", "discover-fwd-0", DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token:   "SmartConsole",
			Members: members(dupBPath, "kb/other-x.md"),
		},
	}, -1)
	// Refs must cite EVERY seed, and the reinforced fact must not be one of
	// them — a fact is not an independent derivation of itself.
	env.applyResponse(discoverID, fmt.Sprintf(
		`{"proposals":[],"reinforcements":[{"path":%q,"reason":"the seeds independently re-derive this",
		  "refs":[%q,%q]}]}`, dupAPath, dupBPath, "kb/other-x.md"))

	live, err := env.svc.Search().GetByPath(context.Background(), env.branch, dupAPath)
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Greater(t, live.Sources, before,
		"fixture: the reinforcement must actually have landed, or this proves nothing")

	after := sourcesOf(env.factsOf(queuedID), dupAPath)
	require.Equal(t, live.Sources, after,
		"a reinforced fact's queued snapshot must show the sources the corpus now holds")
}

func sourcesOf(facts []factForLLM, path string) int {
	for _, f := range facts {
		if f.File == path {
			return f.Sources
		}
	}
	return -1
}
