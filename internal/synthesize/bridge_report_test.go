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
	// For singleClusterTok: enumerateBridgeCandidates won't produce a candidate (same community),
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
func TestBridgeComponentReport_SameCommunityToken_ProducesNoCandidates(t *testing.T) {
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

	// Both in same cluster → enumerateBridgeCandidates will NOT produce a bridge candidate.
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

	// enumerateBridgeCandidates returns nothing for same-community → scoring functions
	// MUST NEVER be called. No expectations are set for SimilarityAdjacency/
	// ReverseDependentPaths/TokenDF, so any such call would fail the test.

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
	require.Empty(t, results, "same-community token yields no bridge candidates")
}

// TestBridgeComponentReport_CrossCommunityLowCohesion_GatedNotKept proves the
// CohFloor gate fires INSIDE BridgeComponentReport: a genuine cross-community
// candidate (so enumerateBridgeCandidates keeps it) whose members have no SIMILAR_TO edges
// → cohesion 0 < CohFloor → the candidate appears in results but Kept=false,
// Q=0 (the gate path of bridgeQ returns (0, false)).
func TestBridgeComponentReport_CrossCommunityLowCohesion_GatedNotKept(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	branch := "agent/test"

	// Two facts sharing entity "gappy" in DIFFERENT communities → a real
	// cross-community bridge candidate that enumerateBridgeCandidates keeps.
	searchResults := []store.SearchResult{
		makeSearchResult("kb/p.md", "P", "body p", "synthesis", "authored", nil, []string{"gappy"}, 0.9, 1),
		makeSearchResult("kb/q.md", "Q", "body q", "synthesis", "authored", nil, []string{"gappy"}, 0.8, 1),
	}
	cr := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/p.md"},
			1: {"kb/q.md"},
		},
	}

	idx.EXPECT().Search(gomock.Any(), branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(searchResults, nil).Times(1)
	idx.EXPECT().CachedClusterFacts(gomock.Any(), branch, gomock.Any(), gomock.Any()).
		Return(cr, nil).Times(1)

	// No SIMILAR_TO edges between members → cohesion 0 < CohFloor (0.5).
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		Return(store.NewSimilarityGraph(nil), nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(2, nil).AnyTimes()

	cfg := QualityConfig{
		CohFloor:     0.5, // cohesion 0 fails this gate
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, BridgeEntity, EffortHigh, 1.0, 1, cfg)
	require.NoError(t, err)

	// The candidate IS produced (cross-community) but gated by CohFloor.
	var gated *ScoredBridge
	for i := range results {
		if results[i].Token == "gappy" {
			gated = &results[i]
			break
		}
	}
	require.NotNil(t, gated, "cross-community candidate 'gappy' must be present in results")
	require.False(t, gated.Kept, "low-cohesion candidate must be gated out (Kept=false)")
	require.Equal(t, 0.0, gated.Q, "gated candidate Q must be 0 (CohFloor gate path)")
	require.Less(t, gated.Comp.Coh, cfg.CohFloor, "cohesion must be below CohFloor to prove the gate fired")
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
// produces no scored bridges (BridgeComponentReport returns early at normal effort).
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
	require.Empty(t, results, "EffortNormal must return empty results (effort gate returns early)")
}
