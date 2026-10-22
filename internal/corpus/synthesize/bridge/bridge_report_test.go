package bridge

import (
	"context"
	"errors"
	"testing"

	"knomit/internal/corpus/synthesize/internal/synthtest"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/index"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/facts"
)

// makeSearchResult builds a store.SearchResult for test fixtures.
func makeSearchResult(path, title, body, typ, origin string, domains, entities []string, confidence float64, sources int) index.SearchResult {
	return index.SearchResult{
		FactWithBody: facts.FactWithBody{
			FactRecord: facts.FactRecord{
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
	branch := "agent/test"

	// Two synthesis facts in different clusters sharing entity "bridgeTok".
	// Two synthesis facts sharing entity "singleClusterTok" in same cluster.
	searchResults := []index.SearchResult{
		makeSearchResult("kb/a.md", "A", "body a", "synthesis", "authored", nil, []string{"bridgeTok"}, 0.9, 2),
		makeSearchResult("kb/b.md", "B", "body b", "synthesis", "authored", nil, []string{"bridgeTok"}, 0.8, 1),
		makeSearchResult("kb/c.md", "C", "body c", "synthesis", "authored", nil, []string{"singleClusterTok"}, 0.7, 1),
		makeSearchResult("kb/d.md", "D", "body d", "synthesis", "authored", nil, []string{"singleClusterTok"}, 0.7, 1),
	}

	// a,c,d cluster together; b stands alone → bridgeTok ({a,b}) forms a
	// cross-community bridge, singleClusterTok ({c,d}) does not. a↔b are
	// similar (a real SIMILAR_TO edge) → cohesion = 1.0 (one pair, one edge);
	// nothing cites anything → gap = 1.0; bridgeTok is carried by 2 live facts.
	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/a.md", "kb/c.md", "kb/d.md"}, {"kb/b.md"}}).
		Similar([2]string{"kb/a.md", "kb/b.md"}).Allow("ReverseDependentPaths", "SimilarityAdjacency", "TokenDF", "Search", "SubgraphEdges").Idx()

	cfg := QualityConfig{
		CohFloor:     0.5,
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortHigh, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
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
	branch := "agent/test"

	// Two facts sharing entity "shared" but both in community 0 → no bridge.
	searchResults := []index.SearchResult{
		makeSearchResult("kb/x.md", "X", "body x", "synthesis", "authored", nil, []string{"shared"}, 0.8, 1),
		makeSearchResult("kb/y.md", "Y", "body y", "synthesis", "authored", nil, []string{"shared"}, 0.8, 1),
	}

	// Both in same cluster → EnumerateBridgeCandidates will NOT produce a bridge candidate.
	// x,y in one community → their shared token is not a cross-community bridge,
	// so the scoring reads (SimilarityAdjacency, ReverseDependentPaths, TokenDF)
	// MUST NEVER happen: the strict policy allows only the clustering reads.
	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/x.md", "kb/y.md"}}).Idx()
	synthtest.GuardReads(t, idx, synthtest.ReadPolicy{Allow: []string{"Search", "SubgraphEdges"}})

	cfg := QualityConfig{
		CohFloor:     0.5,
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortHigh, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
	require.NoError(t, err)
	require.Empty(t, results, "same-community token yields no bridge candidates")
}

// TestBridgeComponentReport_CrossCommunityLowCohesion_GatedNotKept proves the
// CohFloor gate fires INSIDE BridgeComponentReport: a genuine cross-community
// candidate (so EnumerateBridgeCandidates keeps it) whose members have no SIMILAR_TO edges
// → cohesion 0 < CohFloor → the candidate appears in results but Kept=false,
// Q=0 (the gate path of bridgeQ returns (0, false)).
func TestBridgeComponentReport_CrossCommunityLowCohesion_GatedNotKept(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	// Two facts sharing entity "gappy" in DIFFERENT communities → a real
	// cross-community bridge candidate that EnumerateBridgeCandidates keeps.
	searchResults := []index.SearchResult{
		makeSearchResult("kb/p.md", "P", "body p", "synthesis", "authored", nil, []string{"gappy"}, 0.9, 1),
		makeSearchResult("kb/q.md", "Q", "body q", "synthesis", "authored", nil, []string{"gappy"}, 0.8, 1),
	}
	// p,q in distinct communities → their shared token bridges, but with no
	// SIMILAR_TO edge between them cohesion is 0 and the candidate is gated out.
	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/p.md"}, {"kb/q.md"}}).Allow("ReverseDependentPaths", "SimilarityAdjacency", "TokenDF", "Search", "SubgraphEdges").Idx()

	cfg := QualityConfig{
		CohFloor:     0.5, // cohesion 0 fails this gate
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortHigh, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
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
	branch := "agent/test"

	// Two tokens across different clusters: "alpha" (cohesive) and "beta" (not cohesive)
	searchResults := []index.SearchResult{
		makeSearchResult("kb/a1.md", "A1", "b", "synthesis", "authored", nil, []string{"alpha"}, 0.9, 1),
		makeSearchResult("kb/a2.md", "A2", "b", "synthesis", "authored", nil, []string{"alpha"}, 0.9, 1),
		makeSearchResult("kb/b1.md", "B1", "b", "synthesis", "authored", nil, []string{"beta"}, 0.9, 1),
		makeSearchResult("kb/b2.md", "B2", "b", "synthesis", "authored", nil, []string{"beta"}, 0.9, 1),
	}

	// alpha has an edge → cohesion 1.0; beta has none → cohesion 0 (gated out by CohFloor=0.5)
	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/a1.md", "kb/b1.md"}, {"kb/a2.md", "kb/b2.md"}}).
		Similar([2]string{"kb/a1.md", "kb/a2.md"}).Allow("ReverseDependentPaths", "SimilarityAdjacency", "TokenDF", "Search", "SubgraphEdges").Idx()

	cfg := QualityConfig{
		CohFloor:     0.5, // beta (cohesion=0) gated out
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         0.5,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	results, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortHigh, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
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
	branch := "agent/test"

	searchResults := []index.SearchResult{
		makeSearchResult("kb/a.md", "A", "b", "synthesis", "authored", nil, []string{"tok"}, 0.9, 1),
		makeSearchResult("kb/b.md", "B", "b", "synthesis", "authored", nil, []string{"tok"}, 0.8, 1),
	}
	boom := errors.New("adjacency unavailable")

	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/a.md"}, {"kb/b.md"}}).Idx()
	synthtest.GuardReads(t, idx, synthtest.ReadPolicy{AllowAll: true, Fail: map[string]error{"SimilarityAdjacency": boom}})

	cfg := QualityConfig{CohFloor: 0.0, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}

	_, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortHigh, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
}

// TestBridgeComponentReport_NormalEffort_Empty verifies that EffortNormal
// produces no scored bridges (BridgeComponentReport returns early at normal effort).
func TestBridgeComponentReport_NormalEffort_Empty(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	searchResults := []index.SearchResult{
		makeSearchResult("kb/a.md", "A", "b", "synthesis", "authored", nil, []string{"tok"}, 0.9, 1),
		makeSearchResult("kb/b.md", "B", "b", "synthesis", "authored", nil, []string{"tok"}, 0.8, 1),
	}
	// ScopedCluster runs before the effort gate, so its reads happen; the scoring
	// reads must NOT happen at normal effort (the strict policy fails on them).
	idx := synthtest.NewGraphEnv(t, branch).SeedResults(searchResults).
		Partition([][]string{{"kb/a.md"}, {"kb/b.md"}}).Idx()
	synthtest.GuardReads(t, idx, synthtest.ReadPolicy{Allow: []string{"Search", "SubgraphEdges"}})

	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	results, err := BridgeComponentReport(ctx, idx, branch, testLocalRepoID, BridgeEntity, step.EffortNormal, 1.0, 1, synthtest.DefaultNeighborKinds, cfg)
	require.NoError(t, err)
	require.Empty(t, results, "EffortNormal must return empty results (effort gate returns early)")
}
