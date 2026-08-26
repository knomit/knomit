package synthesize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// The prune→distill progression (knomit#135).
//
// Distill used to be planned ALONGSIDE prune at session start, so a cluster was
// reasoned over twice in one session — and a cluster whose prune verdict MERGED
// two facts was still distilled from the pre-merge grouping, which is how
// synthesis fabricated corroboration out of one claim recorded twice.

func pruneItem(clusterKey string, paths ...string) *store.PipelineWorkItem {
	facts := make([]factForLLM, 0, len(paths))
	for _, p := range paths {
		facts = append(facts, factForLLM{File: p, Title: "t " + p, Body: "b " + p, Type: "observation"})
	}
	payload, err := json.Marshal(facts)
	if err != nil {
		panic(err)
	}
	return &store.PipelineWorkItem{
		StepType: "prune", ClusterKey: clusterKey, FactsJSON: string(payload),
	}
}

func keeps(paths ...string) PruneResult {
	d := make([]PruneDecision, 0, len(paths))
	for _, p := range paths {
		d = append(d, PruneDecision{Path: p, Action: "keep"})
	}
	return PruneResult{Decisions: d}
}

// TestProgression_ExplicitCompleteAllKeepCertifies is the promotion signal.
func TestProgression_ExplicitCompleteAllKeepCertifies(t *testing.T) {
	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md"}
	require.True(t, pruneVerdictCertifiesDistinct(keeps(paths...), paths),
		"an explicit keep for every fact in the item is the judge certifying the "+
			"cluster distinct — synthesis's legitimate input")
}

// TestProgression_EmptyDecisionsIsNotAllKeep is the one that protects MN5.
//
// `{"decisions":[],"merges":[]}` is a NON-ANSWER, not an all-KEEP. Promoting on
// it manufactures synthesis work out of silence — and it is exactly what the
// EffortNormal regression fixture answers prune with, so reading empty as
// all-KEEP would make that fixture start producing distill items.
func TestProgression_EmptyDecisionsIsNotAllKeep(t *testing.T) {
	paths := []string{"kb/a.md", "kb/b.md"}
	require.False(t, pruneVerdictCertifiesDistinct(PruneResult{}, paths),
		"an empty decision list is silence, and synthesis is never planned on silence")
	require.False(t, pruneVerdictCertifiesDistinct(PruneResult{Decisions: []PruneDecision{}}, paths),
		"an explicitly empty list is the same non-answer as a missing one")

	// AND with no input paths either. This is the case that separates the
	// explicit empty-decisions guard from the coverage loop below it: with a
	// non-empty item the coverage loop already rejects an empty answer, so
	// deleting the guard leaves every other assertion here green (sabotage S2).
	// Only a zero-fact item tells the two apart — both loops run zero times, and
	// without the guard the predicate returns TRUE, certifying nothing at all as
	// distinct.
	//
	// The downstream `len(facts) < 2` check in promoteClusterToDistill means
	// that would not actually enqueue anything today. The guard stays anyway:
	// a predicate that is correct only because of what its caller happens to do
	// next is a trap for the next caller.
	require.False(t, pruneVerdictCertifiesDistinct(PruneResult{}, nil),
		"a verdict over NO facts certifies nothing — vacuous truth is not a "+
			"judge's classification")
}

// TestProgression_PartialCoverageIsNotAllKeep — validatePrunePaths accepts a
// PARTIAL decision list (it checks that decisions name known paths, never that
// known paths have decisions), so "every decision was keep" is satisfied by a
// judge that classified one fact out of three. Partial coverage is silence for
// the rest.
func TestProgression_PartialCoverageIsNotAllKeep(t *testing.T) {
	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md"}
	partial := keeps("kb/a.md", "kb/b.md")

	// Fixture assertion: the partial answer is genuinely VALID input, or this
	// test is asserting against something the pipeline would have rejected
	// upstream and the coverage check is being credited for the validator's work.
	require.NoError(t, validatePrunePaths(partial, paths),
		"fixture must be a verdict the validator ACCEPTS, or the coverage check "+
			"under test is unreachable in production")

	require.False(t, pruneVerdictCertifiesDistinct(partial, paths),
		"a cluster with an unclassified fact has not been certified distinct")
}

// TestProgression_AnyActionStopsTheCluster — merge, retract and update all ACT.
// Each produces a new content-addressed row, so the facts re-seed through the
// watermark and the cluster belongs to NEXT session, on settled material.
func TestProgression_AnyActionStopsTheCluster(t *testing.T) {
	paths := []string{"kb/a.md", "kb/b.md"}

	retract := keeps(paths...)
	retract.Decisions[1].Action = "retract"
	require.False(t, pruneVerdictCertifiesDistinct(retract, paths),
		"a retract removes a fact; the cluster stops here")

	update := keeps(paths...)
	update.Decisions[1].Action = "update"
	require.False(t, pruneVerdictCertifiesDistinct(update, paths),
		"an UPDATE rewrites a fact into a new row, so it dirties exactly as a "+
			"retract does — distilling over a fact that just changed underneath is "+
			"what the progression exists to prevent")

	merged := keeps(paths...)
	merged.Merges = []MergeEntry{{Paths: paths}}
	require.False(t, pruneVerdictCertifiesDistinct(merged, paths),
		"a merge is the judge saying these facts overlap — the exact case that "+
			"fabricated corroboration when distill ran anyway")
}

// TestProgression_ShortlistItemsDoNotPromote — a `restate-N` item is a two-fact
// cross-cluster PAIR. Its all-KEEP means "these two restatements are distinct",
// which is a judgement about redundancy between two specific facts, NOT the
// claim that some group is a coherent subject worth synthesizing upward from.
func TestProgression_ShortlistItemsDoNotPromote(t *testing.T) {
	cluster := pruneItem("cluster-3", "kb/a.md", "kb/b.md")
	require.True(t, promotesToDistill(cluster),
		"a cluster-shaped prune item is the promotable kind")

	shortlist := pruneItem(restatementClusterKeyPrefix+"0", "kb/a.md", "kb/b.md")
	require.False(t, promotesToDistill(shortlist),
		"a shortlist pair's all-KEEP says the two are distinct, not that a group "+
			"is worth synthesizing")

	// Fixture assertion: the two differ ONLY in cluster key, so the exclusion is
	// credited to the key and not to some other difference.
	require.Equal(t, cluster.StepType, shortlist.StepType)
	require.Equal(t, cluster.FactsJSON, shortlist.FactsJSON)
}

// TestProgression_NonPruneItemsDoNotPromote — the hook is scoped to the prune
// arm, but the predicate states it too, so a future caller cannot widen it by
// accident.
func TestProgression_NonPruneItemsDoNotPromote(t *testing.T) {
	require.False(t, promotesToDistill(&store.PipelineWorkItem{StepType: "distill", ClusterKey: "distill-c0"}))
	require.False(t, promotesToDistill(&store.PipelineWorkItem{StepType: "discover", ClusterKey: "discover-0"}))
	require.False(t, promotesToDistill(nil))
}

// TestProgression_RemainderIsPlannedAtSessionStart pins ruling 1, and it is the
// coverage the naive reading of #135 silently drops.
//
// distillGroups' remainder is the seeds that landed in no multi-seed cluster.
// They never become a prune item, so they have NO verdict to be classified by —
// gating them on promotion would gate them on nothing. They were never part of
// the double-reasoning #135 removes: that was distill firing on prune's OWN
// clusters.
func TestProgression_RemainderIsPlannedAtSessionStart(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/x/1.md"}, {File: "kb/x/2.md"}, // one clustered pair
		{File: "kb/lonely.md"}, // in no cluster at all
	}
	clusters := [][]factForLLM{{seeds[0], seeds[1]}}

	groups := distillGroups(seeds, clusters)

	var clustered, remainder []distillGroup
	for _, g := range groups {
		if g.Remainder {
			remainder = append(remainder, g)
		} else {
			clustered = append(clustered, g)
		}
	}

	// Fixture assertion: BOTH kinds must exist, or the split is not being tested.
	require.Len(t, clustered, 1, "fixture must produce a clustered group")
	require.Len(t, remainder, 1, "fixture must produce a remainder group")

	require.Equal(t, "kb/lonely.md", remainder[0].Facts[0].File,
		"the unclustered seed belongs to the remainder, which is NOT promotion-gated "+
			"— it has no prune verdict to be gated on")
}

// TestProgression_PromotedKeyIsDistinguishable — a promoted item must be
// tellable from a planned one in the queue census, or an auditor cannot see the
// progression working.
func TestProgression_PromotedKeyIsDistinguishable(t *testing.T) {
	require.True(t, strings.HasPrefix(promotedDistillKeyPrefix, "distill-"),
		"a promoted item is still a distill item and should read as one")
	require.NotEqual(t, "distill-rest", promotedDistillKeyPrefix)
	require.False(t, strings.HasPrefix("distill-c0", promotedDistillKeyPrefix),
		"a plan-time clustered key must not be mistaken for a promoted one")
	require.False(t, strings.HasPrefix("distill-rest", promotedDistillKeyPrefix),
		"the remainder key must not be mistaken for a promoted one")
}

// TestProgression_StopIsStatedNotInferred — absence of work must be STATED. A
// cluster that stopped because its judge acted on it must not be
// indistinguishable from a cluster the progression forgot.
func TestProgression_StopIsStatedNotInferred(t *testing.T) {
	item := pruneItem("cluster-1", "kb/a.md", "kb/b.md")

	acted := &store.PipelineSession{}
	recordProgressionStop(acted, item, &ReviewStats{Merged: 1})
	require.Len(t, acted.Health, 1)
	require.Contains(t, acted.Health[0], "ACTED on")
	require.Contains(t, acted.Health[0], "NEXT session",
		"the line must say where the cluster went, not merely that it stopped")

	silent := &store.PipelineSession{}
	recordProgressionStop(silent, item, &ReviewStats{})
	require.Len(t, silent.Health, 1)
	require.Contains(t, silent.Health[0], "not promoted")
	require.NotContains(t, silent.Health[0], "ACTED on",
		"an incomplete verdict is a different outcome from an action and must not "+
			"be reported as one")
}

// ── the call site ─────────────────────────────────────────────────────────
//
// Everything above tests the PREDICATE. A predicate that works proves the
// predicate works, not that anything calls it — the campaign's standing check
// #3. These drive the real ContinueSession → Decode → Apply path and read the
// resulting queue.

// progressionEnv seeds two real facts and puts a prune item in front of the
// judge, without going through Plan.
//
// Deliberately NOT via Plan: at the production Louvain resolution no cluster
// forms in this harness (the same wall knomit#149 hit), so a Plan-driven
// fixture would produce no prune item and every assertion below would pass
// vacuously against an empty queue.
func progressionEnv(t *testing.T, clusterKey string) (*restatementEnv, *Pipeline, *store.PipelineSession) {
	t.Helper()
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/prog/alpha.md", "alpha", "body of alpha")
	env.writeFact("kb/prog/beta.md", "beta", "body of beta")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch, "")
	require.NoError(t, err)

	facts := []factForLLM{
		{File: "kb/prog/alpha.md", Title: "alpha", Body: "body of alpha", Type: "observation"},
		{File: "kb/prog/beta.md", Title: "beta", Body: "body of beta", Type: "observation"},
	}
	payload, err := json.Marshal(facts)
	require.NoError(t, err)
	require.NoError(t, env.svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: clusterKey,
		FactsJSON: string(payload), Priority: 2,
	}))

	p := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{}, reviewStrategy{})
	return env, p, sess
}

func promotedItems(t *testing.T, env *restatementEnv, sessionID string) []store.PipelineWorkItem {
	t.Helper()
	items, err := env.svc.Pipeline().PendingPipelineWorkItems(context.Background(), sessionID)
	require.NoError(t, err)
	var out []store.PipelineWorkItem
	for _, it := range items {
		if strings.HasPrefix(it.ClusterKey, promotedDistillKeyPrefix) {
			out = append(out, it)
		}
	}
	return out
}

const allKeepAnswer = `{"decisions":[{"path":"kb/prog/alpha.md","action":"keep"},` +
	`{"path":"kb/prog/beta.md","action":"keep"}],"merges":[]}`

// TestProgression_AllKeepPromotesThroughTheRealApplyPath is the whole fix,
// end to end.
func TestProgression_AllKeepPromotesThroughTheRealApplyPath(t *testing.T) {
	ctx := context.Background()
	env, p, sess := progressionEnv(t, "cluster-0")

	res, err := p.ContinueSession(ctx, sess.ID, allKeepAnswer)
	require.NoError(t, err)

	promoted := promotedItems(t, env, sess.ID)
	require.Len(t, promoted, 1,
		"an all-KEEP cluster must get its distill item IN THIS SESSION — a "+
			"'next session' promise never fires, because an all-KEEP dirties "+
			"nothing and the watermark never re-seeds it")
	require.Equal(t, "distill", promoted[0].StepType)
	require.Contains(t, promoted[0].FactsJSON, "kb/prog/alpha.md")
	require.Contains(t, promoted[0].FactsJSON, "kb/prog/beta.md")

	require.Contains(t, strings.Join(res.Health, "\n"), "came back all-KEEP",
		"the promotion must ride the turn that produced it")
	require.Contains(t, strings.Join(res.Health, "\n"), "WHOLE CLUSTER",
		"the payload widening is a designed consequence and must be legible to "+
			"an auditor reading distill output, not left to be discovered")
}

// TestProgression_ActedVerdictPromotesNothing — the acted-on case is the one
// that fabricated corroboration when distill ran anyway.
func TestProgression_ActedVerdictPromotesNothing(t *testing.T) {
	ctx := context.Background()
	env, p, sess := progressionEnv(t, "cluster-0")

	retract := `{"decisions":[{"path":"kb/prog/alpha.md","action":"keep"},` +
		`{"path":"kb/prog/beta.md","action":"retract"}],"merges":[]}`
	res, err := p.ContinueSession(ctx, sess.ID, retract)
	require.NoError(t, err)

	require.Empty(t, promotedItems(t, env, sess.ID),
		"an acted-on cluster STOPS this session; its facts are now dirty and it "+
			"is reconsidered next session on settled material")
	require.Contains(t, strings.Join(res.Health, "\n"), "ACTED on",
		"and the stop must be STATED — a cluster that stopped by design must not "+
			"read the same as one the progression forgot")
}

// TestProgression_EmptyAnswerPromotesNothingThroughApply is MN5's own answer
// shape, driven through the real path.
func TestProgression_EmptyAnswerPromotesNothingThroughApply(t *testing.T) {
	ctx := context.Background()
	env, p, sess := progressionEnv(t, "cluster-0")

	res, err := p.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)

	require.Empty(t, promotedItems(t, env, sess.ID),
		"an empty decision list is silence, not an all-KEEP — promoting on it "+
			"would manufacture synthesis work from a non-answer, and would make "+
			"the EffortNormal fixture start producing distill items")
	require.Contains(t, strings.Join(res.Health, "\n"), "not promoted")
}

// TestProgression_ShortlistItemPromotesNothingThroughApply — same all-KEEP
// answer, same facts, only the cluster key differs.
func TestProgression_ShortlistItemPromotesNothingThroughApply(t *testing.T) {
	ctx := context.Background()
	env, p, sess := progressionEnv(t, restatementClusterKeyPrefix+"0")

	_, err := p.ContinueSession(ctx, sess.ID, allKeepAnswer)
	require.NoError(t, err)

	require.Empty(t, promotedItems(t, env, sess.ID),
		"a shortlist pair's all-KEEP means 'these two restatements are distinct', "+
			"which is not 'this group is worth synthesizing over'")
}

// ── the plan side, driven through the real Plan ───────────────────────────

// edgeReadFails forces ScopedCluster onto its DOCUMENTED category-grouping
// fallback, which is the only way to get a real cluster out of this harness.
//
// At the production Louvain resolution no community survives
// filterSmallClusters on a test-sized corpus — measured at 4, 8, 15 and 30
// tightly-grouped facts, all yielding zero clusters, which is the same wall
// knomit#149 hit. Without a cluster there is no clustered distill group, so a
// test of the promotion GATE would pass against a queue that never had a
// `distill-c*` item to suppress: vacuous, and it stayed green under the
// sabotage that removed the gate entirely (S1).
//
// Failing the edge read is not a contrivance — it is a path ScopedCluster
// documents and handles, and it groups the subgraph by category directory,
// which for a single-directory fixture is exactly one cluster.
type edgeReadFails struct{ SearchQuery }

func (edgeReadFails) SubgraphEdges(context.Context, []string) ([][2]string, error) {
	return nil, errors.New("test: forced subgraph edge-read failure")
}

// planFixture seeds n facts in ONE directory and plans a session over them with
// clustering forced through the fallback.
func planFixture(t *testing.T, n int) (*restatementEnv, *store.PipelineSession, []store.PipelineWorkItem) {
	t.Helper()
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	var seeds []fact.Fact
	for i := range n {
		path := fmt.Sprintf("kb/topic/f%d.md", i)
		env.writeFact(path, fmt.Sprintf("note %d", i), "body")
		r, err := env.svc.Facts().ReadFact(ctx, env.branch, path, nil)
		require.NoError(t, err)
		f, err := fact.ParseFact(path, r.Content)
		require.NoError(t, err)
		seeds = append(seeds, f)
	}

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch, "")
	require.NoError(t, err)
	d := env.deps()
	d.Search = edgeReadFails{env.svc.Search()}
	require.NoError(t, reviewStrategy{}.Plan(ctx, d, sess, seeds))

	items, err := env.svc.Pipeline().PendingPipelineWorkItems(ctx, sess.ID)
	require.NoError(t, err)
	return env, sess, items
}

func keysWithPrefix(items []store.PipelineWorkItem, prefix string) []string {
	var out []string
	for _, it := range items {
		if strings.HasPrefix(it.ClusterKey, prefix) {
			out = append(out, it.ClusterKey)
		}
	}
	return out
}

// TestProgression_PlanDoesNotEnqueueClusteredDistillUpFront is the plan half of
// #135, driven through the real Plan.
//
// The fixture is asserted to produce BOTH a prune item and (before the fix) a
// clustered distill group, so "no distill-c* in the queue" is a statement about
// suppression rather than about an empty queue.
func TestProgression_PlanDoesNotEnqueueClusteredDistillUpFront(t *testing.T) {
	_, _, items := planFixture(t, 4)

	// Fixture assertion: a cluster REALLY formed, or there was never a
	// clustered distill group to suppress and this test measures nothing.
	require.NotEmpty(t, keysWithPrefix(items, "cluster-"),
		"fixture must produce a prune item, or no cluster formed and the gate "+
			"under test is unreachable")

	require.Empty(t, keysWithPrefix(items, "distill-c"),
		"a clustered distill item must NOT be planned at session start — the "+
			"cluster is reasoned over by prune first, and its distill item exists "+
			"only if the verdict earns it")
}

// TestProgression_PlanStillEnqueuesTheRemainder — ruling 1, at the call site.
// The remainder has no prune item to be gated on, so gating it would be gating
// it on nothing.
func TestProgression_PlanStillEnqueuesTheRemainder(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Two facts in one directory (they cluster via the fallback) plus one
	// filed alone, which the category grouping cannot pair with anything.
	paths := []string{"kb/topic/a.md", "kb/topic/b.md", "kb/alone/c.md"}
	var seeds []fact.Fact
	for _, p := range paths {
		env.writeFact(p, "note "+p, "body")
		r, err := env.svc.Facts().ReadFact(ctx, env.branch, p, nil)
		require.NoError(t, err)
		f, err := fact.ParseFact(p, r.Content)
		require.NoError(t, err)
		seeds = append(seeds, f)
	}
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch, "")
	require.NoError(t, err)
	d := env.deps()
	d.Search = edgeReadFails{env.svc.Search()}
	require.NoError(t, reviewStrategy{}.Plan(ctx, d, sess, seeds))

	items, err := env.svc.Pipeline().PendingPipelineWorkItems(ctx, sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, keysWithPrefix(items, "distill-rest"),
		"the remainder is NOT promotion-gated: those seeds never become a prune "+
			"item, so there is no verdict to gate them on and gating them would "+
			"drop them silently")
}
