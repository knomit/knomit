package synthesize

import (
	"context"
	"knomit/internal/corpus/synthesize/bridge"
	"knomit/internal/corpus/synthesize/graph"
	"knomit/internal/corpus/synthesize/internal/synthtest"
	"testing"

	"knomit/internal/engine/store/index"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
)

// Neighbour-kind filtering (knomit#308).
//
// Seeds were already epistemic-only (reviewStrategy.AcceptSeed), but the
// searches that GROW a cluster from them were not kind-filtered: a policy filed
// beside an observation, near it in embedding space, was pulled into its
// cluster and dedupCluster merged the two mechanically — rewriting the policy as
// an epistemic fact (mergedFact carries no Kind) and deleting it.
//
// The fix has two halves with two different rules:
//   - CLUSTERING neighbour searches (ScopedCluster's expansion, the prune
//     cousin sweep) admit the kinds in [cluster_cache] neighbor_kinds, default
//     epistemic.
//   - dedupCluster never merges across kinds, whatever that list says.
//
// Every test states a PREMISE first: that the unfiltered QueryByPath search
// really does return the policy. Without it a test where the policy was simply
// never similar enough to be found passes for the wrong reason.

// (a) ScopedCluster's neighbour expansion honours the configured kinds.
func TestNeighborKinds_ScopedClusterExpansion(t *testing.T) {
	ctx := context.Background()
	seeds := func() []step.FactForLLM {
		return []step.FactForLLM{synthtest.EpistemicSeed(synthtest.KindObs), synthtest.EpistemicSeed(synthtest.KindObs2)}
	}

	t.Run("default keeps the policy out", func(t *testing.T) {
		env := synthtest.KindEnv(t, nil)
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindObs, synthtest.KindPol, index.SearchOptions{Path: synthtest.KindDir})
		require.Equal(t, []string{"epistemic"}, env.RI.ClusterNeighborKinds())

		clusters, err := graph.ScopedCluster(ctx, seeds(), synthtest.CompleteEdges(t, env.Svc, env.Branch),
			1.0, 2, env.RI.ClusterNeighborKinds(), nil, env.Branch)
		require.NoError(t, err)
		require.True(t, clustersHoldTogether(clusters, synthtest.KindObs, synthtest.KindObs2),
			"the two observations must cluster, or 'pol is absent' is true of an empty result")
		require.False(t, synthtest.ClusterPaths(clusters)[synthtest.KindPol],
			"a policy must not be pulled into a cluster as an epistemic seed's neighbour")
	})

	t.Run("widened list admits it as a neighbour", func(t *testing.T) {
		env := synthtest.KindEnv(t, synthtest.WidenedNeighborKinds)
		clusters, err := graph.ScopedCluster(ctx, seeds(), synthtest.CompleteEdges(t, env.Svc, env.Branch),
			1.0, 2, env.RI.ClusterNeighborKinds(), nil, env.Branch)
		require.NoError(t, err)
		require.True(t, synthtest.ClusterPaths(clusters)[synthtest.KindPol],
			"with pragmatic listed, the policy is a legitimate neighbour")
		for _, c := range clusters {
			for _, f := range c {
				if f.File == synthtest.KindPol {
					require.Equal(t, string(fact.Pragmatic), f.Kind,
						"a neighbour must carry its kind out of the search, or dedup's guard cannot see it")
				}
			}
		}
	})

	t.Run("empty list is refused, not read as every kind", func(t *testing.T) {
		env := synthtest.KindEnv(t, nil)
		_, err := graph.ScopedCluster(ctx, seeds(), synthtest.CompleteEdges(t, env.Svc, env.Branch), 1.0, 2, nil, nil, env.Branch)
		require.ErrorContains(t, err, "neighbour kinds must not be empty")
	})
}

// (b) dedupCluster never merges across kinds — with the widened list in
// effect, so nothing about the config can be what protects it.
func TestNeighborKinds_DedupNeverMergesAcrossKinds(t *testing.T) {
	ctx := context.Background()

	t.Run("hand-built mixed cluster", func(t *testing.T) {
		// Bypasses ScopedCluster entirely, so the graph/cluster.go filter cannot be
		// what keeps these two apart.
		env := synthtest.KindEnv(t, synthtest.WidenedNeighborKinds)
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindPol, synthtest.KindObs, index.SearchOptions{MinSimilarity: env.DedupThreshold()})
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindObs, synthtest.KindPol, index.SearchOptions{MinSimilarity: env.DedupThreshold()})

		cluster := []step.FactForLLM{
			synthtest.EpistemicSeed(synthtest.KindObs),
			{File: synthtest.KindPol, Kind: string(fact.Pragmatic), Type: string(fact.Policy)},
		}
		surviving, err := dedupCluster(ctx, cluster, env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
			env.DedupThreshold(), step.ReviewTool, func(step.ProgressEvent) {}, env.Branch, testLocalRepoID)
		require.NoError(t, err)
		require.Len(t, surviving, 2, "an observation and a policy are never near-duplicates to mechanical dedup")
		requireParsedOnBranch(t, env.Svc, env.Branch, synthtest.KindObs, fact.Epistemic, fact.Observation)
		requireParsedOnBranch(t, env.Svc, env.Branch, synthtest.KindPol, fact.Pragmatic, fact.Policy)
	})

	t.Run("through the real neighbour path", func(t *testing.T) {
		// Widened config: ScopedCluster puts pol in the cluster with its kind
		// taken from the search hit, and the cluster goes to dedupCluster as
		// review_strategy hands it over.
		env := synthtest.KindEnv(t, synthtest.WidenedNeighborKinds)
		clusters, err := graph.ScopedCluster(ctx, []step.FactForLLM{synthtest.EpistemicSeed(synthtest.KindObs), synthtest.EpistemicSeed(synthtest.KindObs2)},
			synthtest.CompleteEdges(t, env.Svc, env.Branch), 1.0, 2, env.RI.ClusterNeighborKinds(), nil, env.Branch)
		require.NoError(t, err)
		var mixed []step.FactForLLM
		for _, c := range clusters {
			if synthtest.ClusterPaths([][]step.FactForLLM{c})[synthtest.KindPol] {
				mixed = c
			}
		}
		require.NotNil(t, mixed, "PREMISE: the widened cluster must hold the policy")

		surviving, err := dedupCluster(ctx, mixed, env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
			env.DedupThreshold(), step.ReviewTool, func(step.ProgressEvent) {}, env.Branch, testLocalRepoID)
		require.NoError(t, err)
		requireParsedOnBranch(t, env.Svc, env.Branch, synthtest.KindPol, fact.Pragmatic, fact.Policy)
		// "The policy is still live and still a policy" is NOT enough: a
		// cross-kind pair the POLICY wins keeps it pragmatic and deletes the
		// observation into it — the other half of the reproduced corruption.
		// A merge writes the loser's path into the winner's refs, so the
		// policy must have absorbed nothing.
		res, err := env.Svc.Facts().ReadFact(ctx, env.Branch, synthtest.KindPol, nil)
		require.NoError(t, err)
		pol, err := fact.ParseFact(synthtest.KindPol, res.Content)
		require.NoError(t, err)
		for _, r := range pol.Refs {
			require.NotContains(t, r, synthtest.KindObs, "an observation was merged into the policy")
			require.NotContains(t, r, synthtest.KindObs2, "an observation was merged into the policy")
		}
		require.True(t, synthtest.ClusterPaths([][]step.FactForLLM{surviving})[synthtest.KindPol])
		require.Less(t, len(surviving), len(mixed),
			"the two observations must still merge — a dedup that merged nothing proves nothing about the policy")
	})

	t.Run("search asks for epistemic only whatever the config says", func(t *testing.T) {
		env := synthtest.KindEnv(t, synthtest.WidenedNeighborKinds)
		recIdx, rec := synthtest.RecordSearch(t, env.Svc)
		_, err := dedupCluster(ctx, []step.FactForLLM{synthtest.EpistemicSeed(synthtest.KindObs), synthtest.EpistemicSeed(synthtest.KindObs2)},
			env.Svc.Facts(), env.Svc.FactsRepo(), recIdx, env.DedupThreshold(), step.ReviewTool, func(step.ProgressEvent) {}, env.Branch, testLocalRepoID)
		require.NoError(t, err)
		require.NotEmpty(t, rec.Kinds)
		for _, k := range rec.Kinds {
			require.Equal(t, []string{"epistemic"}, k)
		}
		require.False(t, rec.Returned[synthtest.KindPol], "dedup's own search must never return the policy")
	})
}

// A member whose kind a projection site dropped is unmergeable, never
// mergeable: the guard fails closed on "".
func TestSameKnownKind_FailsClosed(t *testing.T) {
	ep := func(p string) step.FactForLLM { return step.FactForLLM{File: p, Kind: "epistemic"} }
	require.True(t, sameKnownKind(ep("a"), ep("b")))
	require.False(t, sameKnownKind(ep("a"), step.FactForLLM{File: "b", Kind: "pragmatic"}))
	require.False(t, sameKnownKind(ep("a"), step.FactForLLM{File: "b"}))
	require.False(t, sameKnownKind(step.FactForLLM{File: "a"}, ep("b")))
	require.False(t, sameKnownKind(step.FactForLLM{File: "a"}, step.FactForLLM{File: "b"}),
		"two unknown kinds are not equal kinds — normalising both to epistemic is the bug")

	t.Run("dedupCluster merges nothing without kinds", func(t *testing.T) {
		env := synthtest.KindEnv(t, nil)
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindObs, synthtest.KindObs2, index.SearchOptions{MinSimilarity: env.DedupThreshold()})
		surviving, err := dedupCluster(context.Background(),
			[]step.FactForLLM{{File: synthtest.KindObs}, {File: synthtest.KindObs2}}, env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
			env.DedupThreshold(), step.ReviewTool, func(step.ProgressEvent) {}, env.Branch, testLocalRepoID)
		require.NoError(t, err)
		require.Len(t, surviving, 2)
		requireParsedOnBranch(t, env.Svc, env.Branch, synthtest.KindObs2, fact.Epistemic, fact.Observation)
	})
}

// (c) The prune cousin sweep uses the same list as the fenced expansion.
func TestNeighborKinds_CousinSweep(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, kinds []string) (*synthtest.RestatementEnv, [][]step.FactForLLM) {
		env := synthtest.KindEnv(t, kinds)
		synthtest.WriteKindedFact(t, env, synthtest.CousinKindObs, synthtest.CousinObsTt, fact.Epistemic, fact.Observation)
		synthtest.WriteKindedFact(t, env, synthtest.CousinKindPol, synthtest.CousinPolTt, fact.Pragmatic, fact.Policy)
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindObs, synthtest.CousinKindPol, index.SearchOptions{MinSimilarity: env.DedupThreshold()})
		// A prune cluster in kindDir that neither cousin belongs to.
		prune := [][]step.FactForLLM{{synthtest.EpistemicSeed(synthtest.KindObs), synthtest.EpistemicSeed(synthtest.KindObs2)}}
		return env, prune
	}

	t.Run("default attaches the epistemic cousin only", func(t *testing.T) {
		env, prune := setup(t, nil)
		joined, h := joinCousinsForPrune(ctx, env.Deps(), env.Branch, prune, env.DedupThreshold())
		require.Empty(t, h.Failure)
		got := synthtest.ClusterPaths(joined)
		require.True(t, got[synthtest.CousinKindObs], "an epistemic cousin in the same place IS attached")
		require.False(t, got[synthtest.CousinKindPol], "a pragmatic cousin is not")
		require.False(t, got[synthtest.KindPol], "nor is the same-directory policy")
	})

	t.Run("widened list attaches the pragmatic cousin", func(t *testing.T) {
		env, prune := setup(t, synthtest.WidenedNeighborKinds)
		joined, h := joinCousinsForPrune(ctx, env.Deps(), env.Branch, prune, env.DedupThreshold())
		require.Empty(t, h.Failure)
		require.True(t, synthtest.ClusterPaths(joined)[synthtest.CousinKindPol])
	})
}

// (d) A caller outside the review deps — the backward bridge path — clusters
// with the list it is handed, not a hard-coded default.
func TestNeighborKinds_BackwardBridgesThreadTheList(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, kinds []string) *synthtest.RecordingSearch {
		env := synthtest.KindEnv(t, kinds)
		// Turn the observations into synthesis facts: the backward path's pool.
		var synth []fact.Fact
		for _, p := range []string{synthtest.KindObs, synthtest.KindObs2} {
			res, err := env.Svc.Facts().ReadFact(ctx, env.Branch, p, nil)
			require.NoError(t, err)
			f, err := fact.ParseFact(p, res.Content)
			require.NoError(t, err)
			f.Type = fact.Synthesis
			f.Origin = fact.Authored
			content, err := fact.SerializeFact(f)
			require.NoError(t, err)
			_, err = env.Svc.Facts().WriteFact(ctx, env.Branch, p, content, "synth "+p, "test")
			require.NoError(t, err)
			synth = append(synth, f)
		}
		synthtest.RequireUnfilteredSearchFinds(t, env, synthtest.KindObs, synthtest.KindPol, index.SearchOptions{Path: synthtest.KindDir})
		recIdx, rec := synthtest.RecordSearch(t, env.Svc)
		_, err := bridge.BuildBackwardBridges(ctx, recIdx, synth, env.Branch, testLocalRepoID, step.EffortHigh,
			bridge.BridgeBoth, 1.0, 2, env.RI.ClusterNeighborKinds(), bridge.QualityConfigFromRepo(env.RI), step.ScopeFilter{})
		require.NoError(t, err)
		require.NotEmpty(t, rec.Kinds, "PREMISE: the bridge path must have run neighbour searches")
		return rec
	}

	t.Run("default", func(t *testing.T) {
		rec := run(t, nil)
		require.False(t, rec.Returned[synthtest.KindPol],
			"no neighbour search the bridge clustering ran returned the policy, so no cluster can hold it")
	})
	t.Run("widened", func(t *testing.T) {
		rec := run(t, synthtest.WidenedNeighborKinds)
		require.True(t, rec.Returned[synthtest.KindPol],
			"the configured list must reach the bridge caller's clustering, not a default")
	})
}

// (e) Widening makes a policy a neighbour, never a seed. This runs the
// full-scan path, so it pins SeedQuery's SQL kind clause; AcceptSeed (the
// incremental path) is covered by review_kind_filter_test.go.
func TestNeighborKinds_WidenedListNeverSeeds(t *testing.T) {
	ctx := context.Background()
	env := synthtest.KindEnv(t, synthtest.WidenedNeighborKinds)
	r := NewReviewer(env.RI, nil)
	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(ctx, env.Branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	paths := seedPaths(seeds)
	require.Contains(t, paths, synthtest.KindObs)
	require.NotContains(t, paths, synthtest.KindPol, "neighbor_kinds widens neighbours only; seeds stay epistemic")
}

// testLocalRepoID is a well-formed 12-hex repo id: a REAL id shape, not "",
// because "" is the configuration under which the lineage exclusion does
// nothing at all.
const testLocalRepoID = "abcdef012345"
