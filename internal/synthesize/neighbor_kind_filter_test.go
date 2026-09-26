package synthesize

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
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

// testNeighborKinds is the production default of [cluster_cache]
// neighbor_kinds, for tests that call a clustering entry point directly.
var testNeighborKinds = []string{"epistemic"}

var widenedNeighborKinds = []string{"epistemic", "pragmatic"}

const (
	kindDir     = "kb/technology/kinds/"
	kindObs     = kindDir + "obs.md"
	kindPol     = kindDir + "pol.md"
	kindObs2    = kindDir + "obs2.md"
	kindTitle   = "Retry the upload after a transient failure"
	kindBody    = "What happens to an upload when the store is briefly unavailable."
	kindPolText = "Always retry the upload after a transient failure"
	kindObs2Tt  = "Uploads are retried after transient store failures"

	// A cousin directory: a sibling of kindDir, so neither is a prefix of the
	// other and ScopedCluster's category fence separates them.
	cousinKindDir = "kb/technology/kinds-elsewhere/"
	cousinKindObs = cousinKindDir + "obs-cousin.md"
	cousinKindPol = cousinKindDir + "pol-cousin.md"
	cousinObsTt   = "An upload is retried when the store fails transiently"
	cousinPolTt   = "Retry every upload that failed transiently"
)

// kindEnv builds a store whose embedder places the facts on one axis:
// obs at 0, pol at 0.01, obs2 at 0.02 — so pol sits BETWEEN the two
// observations, as close to each as they are to each other, and far above the
// dedup floor. neighborKinds is what the repo instance's config says (nil =
// the default).
func kindEnv(t *testing.T, neighborKinds []string) *restatementEnv {
	t.Helper()
	emb := &restatementEmbedder{vectorFor: func(text string) []float32 {
		switch text {
		case kindTitle + " " + kindBody:
			return axisVector(0)
		case kindPolText + " " + kindBody:
			return axisVector(0.01)
		case kindObs2Tt + " " + kindBody:
			return axisVector(0.02)
		case cousinObsTt + " " + kindBody:
			return axisVector(0.005)
		case cousinPolTt + " " + kindBody:
			return axisVector(0.015)
		}
		return nil
	}}
	env := newRestatementEnvWith(t, 0, emb)
	env.ri = repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:          "test",
		AgentBranch:   env.branch,
		Svc:           env.svc,
		OntologyRoot:  "kb",
		Embedder:      emb,
		NeighborKinds: neighborKinds,
	})
	writeKindedFact(t, env, kindObs, kindTitle, fact.Epistemic, fact.Observation)
	writeKindedFact(t, env, kindPol, kindPolText, fact.Pragmatic, fact.Policy)
	writeKindedFact(t, env, kindObs2, kindObs2Tt, fact.Epistemic, fact.Observation)
	return env
}

func writeKindedFact(t *testing.T, env *restatementEnv, path, title string, kind fact.Kind, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = kindBody
	f.Kind = kind
	f.Type = typ
	f.Confidence = 0.7
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, path, content, "write "+path, "test")
	require.NoError(t, err)
}

// requireUnfilteredSearchFinds is the premise every test here rests on: with
// no kind filter, a QueryByPath search from `from` returns `want`.
func requireUnfilteredSearchFinds(t *testing.T, env *restatementEnv, from, want string, opts store.SearchOptions) {
	t.Helper()
	opts.QueryByPath = from
	if opts.Limit == 0 {
		opts.Limit = 10
	}
	hits, err := env.svc.Search().Search(context.Background(), env.branch, opts)
	require.NoError(t, err)
	for _, h := range hits {
		if h.Path == want {
			return
		}
	}
	require.Failf(t, "PREMISE", "an unfiltered search from %s must return %s, or the filter is credited for nothing", from, want)
}

// completeEdges stands in for the SIMILAR_TO edge read. The test store builds
// no SIMILAR_TO edges, so the real SubgraphEdges returns none; Louvain then
// returns one SINGLETON per node — non-empty, so ScopedCluster's category
// fallback never fires — and filterSmallClusters drops every singleton: ZERO
// clusters, over which a "pol is in no cluster" assertion is vacuous. Every
// pair of collected paths is an edge instead, so membership is decided by
// exactly one thing: which paths the neighbour search let into the subgraph.
// The tests also pass resolution 1.0, because the production 4.0 splits even a
// connected 2-node graph.
type completeEdges struct{ SearchQuery }

func (c completeEdges) SubgraphEdges(_ context.Context, paths []string) ([][2]string, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var out [][2]string
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			out = append(out, [2]string{sorted[i], sorted[j]})
		}
	}
	return out, nil
}

// recordingSearch records every path a QueryByPath search returned, and the
// kinds it asked for, so a test can say what a caller's neighbour searches
// could possibly have put into a cluster.
type recordingSearch struct {
	SearchQuery
	mu       sync.Mutex
	returned map[string]bool
	kinds    [][]string
}

func (r *recordingSearch) Search(ctx context.Context, branch string, q store.SearchOptions) ([]store.SearchResult, error) {
	res, err := r.SearchQuery.Search(ctx, branch, q)
	if q.QueryByPath != "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.returned == nil {
			r.returned = map[string]bool{}
		}
		for _, h := range res {
			r.returned[h.Path] = true
		}
		r.kinds = append(r.kinds, append([]string(nil), q.IncludeKinds...))
	}
	return res, err
}

func clusterPaths(clusters [][]factForLLM) map[string]bool {
	out := map[string]bool{}
	for _, c := range clusters {
		for _, f := range c {
			out[f.File] = true
		}
	}
	return out
}

func epistemicSeed(path string) factForLLM {
	return factForLLM{File: path, Kind: string(fact.Epistemic), Type: string(fact.Observation)}
}

// (a) ScopedCluster's neighbour expansion honours the configured kinds.
func TestNeighborKinds_ScopedClusterExpansion(t *testing.T) {
	ctx := context.Background()
	seeds := func() []factForLLM { return []factForLLM{epistemicSeed(kindObs), epistemicSeed(kindObs2)} }

	t.Run("default keeps the policy out", func(t *testing.T) {
		env := kindEnv(t, nil)
		requireUnfilteredSearchFinds(t, env, kindObs, kindPol, store.SearchOptions{Path: kindDir})
		require.Equal(t, []string{"epistemic"}, env.ri.ClusterNeighborKinds())

		clusters, err := ScopedCluster(ctx, seeds(), completeEdges{env.svc.Search()},
			1.0, 2, env.ri.ClusterNeighborKinds(), nil, env.branch)
		require.NoError(t, err)
		require.True(t, clustersHoldTogether(clusters, kindObs, kindObs2),
			"the two observations must cluster, or 'pol is absent' is true of an empty result")
		require.False(t, clusterPaths(clusters)[kindPol],
			"a policy must not be pulled into a cluster as an epistemic seed's neighbour")
	})

	t.Run("widened list admits it as a neighbour", func(t *testing.T) {
		env := kindEnv(t, widenedNeighborKinds)
		clusters, err := ScopedCluster(ctx, seeds(), completeEdges{env.svc.Search()},
			1.0, 2, env.ri.ClusterNeighborKinds(), nil, env.branch)
		require.NoError(t, err)
		require.True(t, clusterPaths(clusters)[kindPol],
			"with pragmatic listed, the policy is a legitimate neighbour")
		for _, c := range clusters {
			for _, f := range c {
				if f.File == kindPol {
					require.Equal(t, string(fact.Pragmatic), f.Kind,
						"a neighbour must carry its kind out of the search, or dedup's guard cannot see it")
				}
			}
		}
	})

	t.Run("empty list is refused, not read as every kind", func(t *testing.T) {
		env := kindEnv(t, nil)
		_, err := ScopedCluster(ctx, seeds(), completeEdges{env.svc.Search()}, 1.0, 2, nil, nil, env.branch)
		require.ErrorContains(t, err, "neighbour kinds must not be empty")
	})
}

// (b) dedupCluster never merges across kinds — with the widened list in
// effect, so nothing about the config can be what protects it.
func TestNeighborKinds_DedupNeverMergesAcrossKinds(t *testing.T) {
	ctx := context.Background()

	t.Run("hand-built mixed cluster", func(t *testing.T) {
		// Bypasses ScopedCluster entirely, so the cluster.go filter cannot be
		// what keeps these two apart.
		env := kindEnv(t, widenedNeighborKinds)
		requireUnfilteredSearchFinds(t, env, kindPol, kindObs, store.SearchOptions{MinSimilarity: env.dedupThreshold()})
		requireUnfilteredSearchFinds(t, env, kindObs, kindPol, store.SearchOptions{MinSimilarity: env.dedupThreshold()})

		cluster := []factForLLM{
			epistemicSeed(kindObs),
			{File: kindPol, Kind: string(fact.Pragmatic), Type: string(fact.Policy)},
		}
		surviving, err := dedupCluster(ctx, cluster, env.svc.Facts(), env.svc.Search(),
			env.dedupThreshold(), reviewTool, func(ProgressEvent) {}, env.branch, testLocalRepoID)
		require.NoError(t, err)
		require.Len(t, surviving, 2, "an observation and a policy are never near-duplicates to mechanical dedup")
		requireParsedOnBranch(t, env.svc, env.branch, kindObs, fact.Epistemic, fact.Observation)
		requireParsedOnBranch(t, env.svc, env.branch, kindPol, fact.Pragmatic, fact.Policy)
	})

	t.Run("through the real neighbour path", func(t *testing.T) {
		// Widened config: ScopedCluster puts pol in the cluster with its kind
		// taken from the search hit, and the cluster goes to dedupCluster as
		// review_strategy hands it over.
		env := kindEnv(t, widenedNeighborKinds)
		clusters, err := ScopedCluster(ctx, []factForLLM{epistemicSeed(kindObs), epistemicSeed(kindObs2)},
			completeEdges{env.svc.Search()}, 1.0, 2, env.ri.ClusterNeighborKinds(), nil, env.branch)
		require.NoError(t, err)
		var mixed []factForLLM
		for _, c := range clusters {
			if clusterPaths([][]factForLLM{c})[kindPol] {
				mixed = c
			}
		}
		require.NotNil(t, mixed, "PREMISE: the widened cluster must hold the policy")

		surviving, err := dedupCluster(ctx, mixed, env.svc.Facts(), env.svc.Search(),
			env.dedupThreshold(), reviewTool, func(ProgressEvent) {}, env.branch, testLocalRepoID)
		require.NoError(t, err)
		requireParsedOnBranch(t, env.svc, env.branch, kindPol, fact.Pragmatic, fact.Policy)
		// "The policy is still live and still a policy" is NOT enough: a
		// cross-kind pair the POLICY wins keeps it pragmatic and deletes the
		// observation into it — the other half of the reproduced corruption.
		// A merge writes the loser's path into the winner's refs, so the
		// policy must have absorbed nothing.
		res, err := env.svc.Facts().ReadFact(ctx, env.branch, kindPol, nil)
		require.NoError(t, err)
		pol, err := fact.ParseFact(kindPol, res.Content)
		require.NoError(t, err)
		for _, r := range pol.Refs {
			require.NotContains(t, r, kindObs, "an observation was merged into the policy")
			require.NotContains(t, r, kindObs2, "an observation was merged into the policy")
		}
		require.True(t, clusterPaths([][]factForLLM{surviving})[kindPol])
		require.Less(t, len(surviving), len(mixed),
			"the two observations must still merge — a dedup that merged nothing proves nothing about the policy")
	})

	t.Run("search asks for epistemic only whatever the config says", func(t *testing.T) {
		env := kindEnv(t, widenedNeighborKinds)
		rec := &recordingSearch{SearchQuery: env.svc.Search()}
		_, err := dedupCluster(ctx, []factForLLM{epistemicSeed(kindObs), epistemicSeed(kindObs2)},
			env.svc.Facts(), rec, env.dedupThreshold(), reviewTool, func(ProgressEvent) {}, env.branch, testLocalRepoID)
		require.NoError(t, err)
		require.NotEmpty(t, rec.kinds)
		for _, k := range rec.kinds {
			require.Equal(t, []string{"epistemic"}, k)
		}
		require.False(t, rec.returned[kindPol], "dedup's own search must never return the policy")
	})
}

// A member whose kind a projection site dropped is unmergeable, never
// mergeable: the guard fails closed on "".
func TestSameKnownKind_FailsClosed(t *testing.T) {
	ep := func(p string) factForLLM { return factForLLM{File: p, Kind: "epistemic"} }
	require.True(t, sameKnownKind(ep("a"), ep("b")))
	require.False(t, sameKnownKind(ep("a"), factForLLM{File: "b", Kind: "pragmatic"}))
	require.False(t, sameKnownKind(ep("a"), factForLLM{File: "b"}))
	require.False(t, sameKnownKind(factForLLM{File: "a"}, ep("b")))
	require.False(t, sameKnownKind(factForLLM{File: "a"}, factForLLM{File: "b"}),
		"two unknown kinds are not equal kinds — normalising both to epistemic is the bug")

	t.Run("dedupCluster merges nothing without kinds", func(t *testing.T) {
		env := kindEnv(t, nil)
		requireUnfilteredSearchFinds(t, env, kindObs, kindObs2, store.SearchOptions{MinSimilarity: env.dedupThreshold()})
		surviving, err := dedupCluster(context.Background(),
			[]factForLLM{{File: kindObs}, {File: kindObs2}}, env.svc.Facts(), env.svc.Search(),
			env.dedupThreshold(), reviewTool, func(ProgressEvent) {}, env.branch, testLocalRepoID)
		require.NoError(t, err)
		require.Len(t, surviving, 2)
		requireParsedOnBranch(t, env.svc, env.branch, kindObs2, fact.Epistemic, fact.Observation)
	})
}

// (c) The prune cousin sweep uses the same list as the fenced expansion.
func TestNeighborKinds_CousinSweep(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, kinds []string) (*restatementEnv, [][]factForLLM) {
		env := kindEnv(t, kinds)
		writeKindedFact(t, env, cousinKindObs, cousinObsTt, fact.Epistemic, fact.Observation)
		writeKindedFact(t, env, cousinKindPol, cousinPolTt, fact.Pragmatic, fact.Policy)
		requireUnfilteredSearchFinds(t, env, kindObs, cousinKindPol, store.SearchOptions{MinSimilarity: env.dedupThreshold()})
		// A prune cluster in kindDir that neither cousin belongs to.
		prune := [][]factForLLM{{epistemicSeed(kindObs), epistemicSeed(kindObs2)}}
		return env, prune
	}

	t.Run("default attaches the epistemic cousin only", func(t *testing.T) {
		env, prune := setup(t, nil)
		joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, prune, env.dedupThreshold())
		require.Empty(t, h.Failure)
		got := clusterPaths(joined)
		require.True(t, got[cousinKindObs], "an epistemic cousin in the same place IS attached")
		require.False(t, got[cousinKindPol], "a pragmatic cousin is not")
		require.False(t, got[kindPol], "nor is the same-directory policy")
	})

	t.Run("widened list attaches the pragmatic cousin", func(t *testing.T) {
		env, prune := setup(t, widenedNeighborKinds)
		joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, prune, env.dedupThreshold())
		require.Empty(t, h.Failure)
		require.True(t, clusterPaths(joined)[cousinKindPol])
	})
}

// (d) A caller outside the review deps — the backward bridge path — clusters
// with the list it is handed, not a hard-coded default.
func TestNeighborKinds_BackwardBridgesThreadTheList(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, kinds []string) *recordingSearch {
		env := kindEnv(t, kinds)
		// Turn the observations into synthesis facts: the backward path's pool.
		var synth []fact.Fact
		for _, p := range []string{kindObs, kindObs2} {
			res, err := env.svc.Facts().ReadFact(ctx, env.branch, p, nil)
			require.NoError(t, err)
			f, err := fact.ParseFact(p, res.Content)
			require.NoError(t, err)
			f.Type = fact.Synthesis
			f.Origin = fact.Authored
			content, err := fact.SerializeFact(f)
			require.NoError(t, err)
			_, err = env.svc.Facts().WriteFact(ctx, env.branch, p, content, "synth "+p, "test")
			require.NoError(t, err)
			synth = append(synth, f)
		}
		requireUnfilteredSearchFinds(t, env, kindObs, kindPol, store.SearchOptions{Path: kindDir})
		rec := &recordingSearch{SearchQuery: env.svc.Search()}
		_, err := BuildBackwardBridges(ctx, rec, synth, env.branch, testLocalRepoID, EffortHigh,
			BridgeBoth, 1.0, 2, env.ri.ClusterNeighborKinds(), QualityConfigFromRepo(env.ri), ScopeFilter{})
		require.NoError(t, err)
		require.NotEmpty(t, rec.kinds, "PREMISE: the bridge path must have run neighbour searches")
		return rec
	}

	t.Run("default", func(t *testing.T) {
		rec := run(t, nil)
		require.False(t, rec.returned[kindPol],
			"no neighbour search the bridge clustering ran returned the policy, so no cluster can hold it")
	})
	t.Run("widened", func(t *testing.T) {
		rec := run(t, widenedNeighborKinds)
		require.True(t, rec.returned[kindPol],
			"the configured list must reach the bridge caller's clustering, not a default")
	})
}

// (e) Widening makes a policy a neighbour, never a seed. This runs the
// full-scan path, so it pins SeedQuery's SQL kind clause; AcceptSeed (the
// incremental path) is covered by review_kind_filter_test.go.
func TestNeighborKinds_WidenedListNeverSeeds(t *testing.T) {
	ctx := context.Background()
	env := kindEnv(t, widenedNeighborKinds)
	r := NewReviewer(env.ri, nil)
	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(ctx, env.branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	paths := seedPaths(seeds)
	require.Contains(t, paths, kindObs)
	require.NotContains(t, paths, kindPol, "neighbor_kinds widens neighbours only; seeds stay epistemic")
}
