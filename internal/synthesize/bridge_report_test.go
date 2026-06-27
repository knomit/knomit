package synthesize

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/store"
)

// makeSearchResult builds a store.SearchResult for test fixtures.
func makeSearchResult(path, title, body, typ, origin string, domains, entities []string, confidence float64, sources int) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{
				Path:       path,
				Title:      title,
				Type:       typ,
				Domain:     domains,
				Entities:   entities,
				Confidence: confidence,
				Sources:    sources,
				Origin:     origin,
			},
			Body: body,
		},
	}
}

// TestBridgeComponentReport_CrossCommunity_Kept verifies that a token shared
// across ≥2 communities with cohesive members is kept (Kept=true, Q>0).
func TestBridgeComponentReport_CrossCommunity_Kept(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	// Two synthesis facts in different clusters sharing entity "bridgeTok".
	// Two synthesis facts sharing entity "singleClusterTok" in same cluster.
	searchResults := []store.SearchResult{
		makeSearchResult("kb/a.md", "A", "body a", "synthesis", "authored", nil, []string{"bridgeTok"}, 0.9, 2),
		makeSearchResult("kb/b.md", "B", "body b", "synthesis", "authored", nil, []string{"bridgeTok"}, 0.8, 1),
		makeSearchResult("kb/c.md", "C", "body c", "synthesis", "authored", nil, []string{"singleClusterTok"}, 0.7, 1),
		makeSearchResult("kb/d.md", "D", "body d", "synthesis", "authored", nil, []string{"singleClusterTok"}, 0.7, 1),
	}

	// a and b in different clusters → bridgeTok forms a cross-community bridge.
	// c and d in same cluster → singleClusterTok is NOT a bridge (same community).
	cr := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/a.md", "kb/c.md", "kb/d.md"},
			1: {"kb/b.md"},
		},
	}

	// a↔b are connected → cohesion = 1.0 (one pair, one edge)
	graphAB := store.NewSimilarityGraph([][2]string{{"kb/a.md", "kb/b.md"}})

	// c↔d: no edges → cohesion = 0
	graphEmpty := store.NewSimilarityGraph(nil)

	idx.EXPECT().Search(gomock.Any(), branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(searchResults, nil).Times(1)

	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)

	// SimilarityAdjacency: for bridgeTok members (a,b) → return graph with a↔b edge.
	// For singleClusterTok: bridgeSeeds won't produce a candidate (same community),
	// but allow AnyTimes to not over-constrain.
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
			for _, p := range paths {
				if p == "kb/a.md" {
					return graphAB, nil
				}
			}
			return graphEmpty, nil
		}).AnyTimes()

	// ReverseDependentPaths: no links → gap = 1.0
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()

	// TokenDF: bridgeTok appears in 2 docs
	idx.EXPECT().TokenDF(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(2, nil).AnyTimes()

	cfg := QualityConfig{
		CohFloor:     0.5,
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortHigh, 1.0, 1, cfg)
	require.NoError(t, err)

	// At least one result: bridgeTok bridge should be kept
	require.NotEmpty(t, results, "expect at least one scored bridge")

	// Find the bridgeTok result
	var bridgeResult *ScoredBridge
	for i := range results {
		if results[i].Token == "bridgeTok" {
			bridgeResult = &results[i]
			break
		}
	}
	require.NotNil(t, bridgeResult, "bridgeTok bridge must be present in results")
	require.True(t, bridgeResult.Kept, "bridgeTok bridge must be Kept=true (cross-community, cohesive)")
	require.Greater(t, bridgeResult.Q, 0.0, "bridgeTok bridge must have Q>0")
	require.Equal(t, BridgeEntity, bridgeResult.Kind)
	require.ElementsMatch(t, []string{"kb/a.md", "kb/b.md"}, bridgeResult.Members)
}

// TestBridgeComponentReport_SameCommunity_NotKept verifies that a token whose
// members are all in one community is gated out (Kept=false, Q=0) or not emitted.
func TestBridgeComponentReport_SameCommunity_NotKept(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	// Two facts sharing entity "shared" but both in community 0 → no bridge.
	searchResults := []store.SearchResult{
		makeSearchResult("kb/x.md", "X", "body x", "synthesis", "authored", nil, []string{"shared"}, 0.8, 1),
		makeSearchResult("kb/y.md", "Y", "body y", "synthesis", "authored", nil, []string{"shared"}, 0.8, 1),
	}

	// Both in same cluster → bridgeSeeds will NOT produce a bridge candidate.
	cr := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/x.md", "kb/y.md"},
		},
	}

	idx.EXPECT().Search(gomock.Any(), branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(searchResults, nil).Times(1)

	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)

	// bridgeSeeds returns nothing for same-community → scoring functions not called,
	// but allow AnyTimes to not over-constrain.
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		Return(store.NewSimilarityGraph(nil), nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(2, nil).AnyTimes()

	cfg := QualityConfig{
		CohFloor:     0.5,
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortHigh, 1.0, 1, cfg)
	require.NoError(t, err)

	// bridgeSeeds produces no cross-community candidates → results empty
	// (or if any are present they must all be Kept=false)
	for _, r := range results {
		require.False(t, r.Kept, "same-community token must not be Kept")
		require.Equal(t, 0.0, r.Q, "gated bridge Q must be 0")
	}
}

// TestBridgeComponentReport_QDescOrdering verifies that results are sorted by
// Q descending, then Token ascending for ties.
func TestBridgeComponentReport_QDescOrdering(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	// Two tokens across different clusters: "alpha" (cohesive) and "beta" (not cohesive)
	searchResults := []store.SearchResult{
		makeSearchResult("kb/a1.md", "A1", "b", "synthesis", "authored", nil, []string{"alpha"}, 0.9, 1),
		makeSearchResult("kb/a2.md", "A2", "b", "synthesis", "authored", nil, []string{"alpha"}, 0.9, 1),
		makeSearchResult("kb/b1.md", "B1", "b", "synthesis", "authored", nil, []string{"beta"}, 0.9, 1),
		makeSearchResult("kb/b2.md", "B2", "b", "synthesis", "authored", nil, []string{"beta"}, 0.9, 1),
	}

	cr := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/a1.md", "kb/b1.md"},
			1: {"kb/a2.md", "kb/b2.md"},
		},
	}

	idx.EXPECT().Search(gomock.Any(), branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(searchResults, nil).Times(1)
	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)

	// alpha has edges → cohesion 1.0; beta has no edges → cohesion 0 (gated out by CohFloor=0.5)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
			for _, p := range paths {
				if p == "kb/a1.md" {
					return store.NewSimilarityGraph([][2]string{{"kb/a1.md", "kb/a2.md"}}), nil
				}
			}
			return store.NewSimilarityGraph(nil), nil
		}).AnyTimes()

	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(2, nil).AnyTimes()

	cfg := QualityConfig{
		CohFloor:     0.5, // beta (cohesion=0) gated out
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         0.5,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortHigh, 1.0, 1, cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results)

	// Output must be sorted Q desc; if equal then Token asc.
	for i := 1; i < len(results); i++ {
		prev, curr := results[i-1], results[i]
		if prev.Q < curr.Q {
			t.Errorf("result[%d].Q=%v < result[%d].Q=%v: must be Q-desc sorted", i-1, prev.Q, i, curr.Q)
		}
		if prev.Q == curr.Q && prev.Token > curr.Token {
			t.Errorf("equal-Q results[%d].Token=%q > results[%d].Token=%q: must be Token-asc on tie", i-1, prev.Token, i, curr.Token)
		}
	}
}

// TestBridgeComponentReport_ErrorPropagation_SimilarityAdjacency verifies that
// an error from SimilarityAdjacency surfaces from BridgeComponentReport.
func TestBridgeComponentReport_ErrorPropagation_SimilarityAdjacency(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	searchResults := []store.SearchResult{
		makeSearchResult("kb/a.md", "A", "b", "synthesis", "authored", nil, []string{"tok"}, 0.9, 1),
		makeSearchResult("kb/b.md", "B", "b", "synthesis", "authored", nil, []string{"tok"}, 0.8, 1),
	}
	cr := store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/a.md"}, 1: {"kb/b.md"}},
	}

	boom := errors.New("adjacency unavailable")

	idx.EXPECT().Search(gomock.Any(), branch, gomock.Any()).Return(searchResults, nil).Times(1)
	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		Return(store.SimilarityGraph{}, boom).AnyTimes()

	cfg := QualityConfig{CohFloor: 0.0, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}

	_, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortHigh, 1.0, 1, cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
}

// TestBridgeComponentReport_NormalEffort_Empty verifies that EffortNormal
// produces no scored bridges (bridgeSeeds is a no-op at normal).
func TestBridgeComponentReport_NormalEffort_Empty(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	searchResults := []store.SearchResult{
		makeSearchResult("kb/a.md", "A", "b", "synthesis", "authored", nil, []string{"tok"}, 0.9, 1),
		makeSearchResult("kb/b.md", "B", "b", "synthesis", "authored", nil, []string{"tok"}, 0.8, 1),
	}
	cr := store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/a.md"}, 1: {"kb/b.md"}},
	}

	idx.EXPECT().Search(gomock.Any(), branch, gomock.Any()).Return(searchResults, nil).Times(1)
	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)
	// No SimilarityAdjacency/TokenDF/ReverseDependentPaths calls at normal effort.

	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	results, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortNormal, 1.0, 1, cfg)
	require.NoError(t, err)
	require.Empty(t, results, "EffortNormal must return empty results (bridgeSeeds is a no-op)")
}
