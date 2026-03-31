package store

import (
	"context"
	"fmt"
	"testing"
)

// TestClusterFacts_SharedEntities verifies that facts sharing the same entities
// are more likely to cluster together than facts with disjoint entities.
func TestClusterFacts_SharedEntities(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-entities"
	idx, gs := openGraphTestStore(t, branch)

	// Group 1: Go + SQLite (3 facts).
	writeAndSync(t, idx, gs, branch, "kb/go1.md",
		makeFact("Go DB Layer", []string{"eng"}, []string{"Go", "SQLite"}))
	writeAndSync(t, idx, gs, branch, "kb/go2.md",
		makeFact("Go DB Tests", []string{"eng"}, []string{"Go", "SQLite"}))

	// Group 2: Python + ML (2 facts).
	writeAndSync(t, idx, gs, branch, "kb/py1.md",
		makeFact("PyTorch Training", []string{"eng"}, []string{"Python", "ML"}))
	writeAndSync(t, idx, gs, branch, "kb/py2.md",
		makeFact("Scikit Pipeline", []string{"eng"}, []string{"Python", "ML"}))

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// All 4 facts should appear in clusters or noise.
	all := clusterPaths(result)
	for _, p := range []string{"kb/go1.md", "kb/go2.md", "kb/py1.md", "kb/py2.md"} {
		if !all[p] {
			t.Errorf("expected %s in results, got %v", p, all)
		}
	}

	// Go facts should be in the same cluster (shared Go+SQLite entities).
	assertSameCluster(t, result, []string{"kb/go1.md", "kb/go2.md"}, "Go+SQLite pair")

	// Python facts should be in the same cluster (shared Python+ML entities).
	assertSameCluster(t, result, []string{"kb/py1.md", "kb/py2.md"}, "Python+ML pair")
}

// TestClusterFacts_SharedDomains verifies that facts in the same leaf domain
// cluster together. Uses shared entities within each domain group to strengthen
// the clustering signal (domains alone may be weaker than OntologyNode edges).
func TestClusterFacts_SharedDomains(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-domains"
	idx, gs := openGraphTestStore(t, branch)

	// Backend group: same leaf domain + shared entity to reinforce clustering.
	writeAndSync(t, idx, gs, branch, "kb/back1.md",
		makeFact("API Server", []string{"eng/backend/api"}, []string{"Backend"}))
	writeAndSync(t, idx, gs, branch, "kb/back2.md",
		makeFact("Auth Handler", []string{"eng/backend/api"}, []string{"Backend"}))

	// Science group: completely separate domain + shared entity.
	writeAndSync(t, idx, gs, branch, "kb/sci1.md",
		makeFact("Physics Sim", []string{"science/physics"}, []string{"Physics"}))
	writeAndSync(t, idx, gs, branch, "kb/sci2.md",
		makeFact("Particle Model", []string{"science/physics"}, []string{"Physics"}))

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Same-domain pairs should cluster.
	assertSameCluster(t, result, []string{"kb/back1.md", "kb/back2.md"}, "backend pair")
	assertSameCluster(t, result, []string{"kb/sci1.md", "kb/sci2.md"}, "science pair")
}

// TestClusterFacts_SemanticSimilarity verifies that facts cluster via SIMILAR_TO
// edges when they have similar embeddings but no shared entities or domains.
func TestClusterFacts_SemanticSimilarity(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-semantic"
	idx, gs := openGraphTestStore(t, branch)
	idx.SetEmbedder(&stubEmbedder768d{})

	// "alpha" and "beta" produce similar vectors (cosine > 0.60).
	// "gamma" is dissimilar to both.
	// No shared entities or domains — clustering must come from SIMILAR_TO.
	writeAndSync(t, idx, gs, branch, "kb/alpha.md",
		makeFact("alpha", []string{"domain-a"}, []string{"EntityA"}))
	writeAndSync(t, idx, gs, branch, "kb/beta.md",
		makeFact("beta", []string{"domain-b"}, []string{"EntityB"}))
	writeAndSync(t, idx, gs, branch, "kb/gamma.md",
		makeFact("gamma", []string{"domain-c"}, []string{"EntityC"}))

	// Build SIMILAR_TO edges.
	for _, path := range []string{"kb/alpha.md", "kb/beta.md", "kb/gamma.md"} {
		bh := blobHash(t, idx, branch, path)
		if err := idx.graphBuildSimilarityEdges(ctx, path, bh); err != nil {
			t.Fatalf("build similarity for %s: %v", path, err)
		}
	}

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// alpha and beta should cluster together via SIMILAR_TO.
	assertSameCluster(t, result, []string{"kb/alpha.md", "kb/beta.md"}, "alpha+beta (similar embeddings)")
}

// TestClusterFacts_NoiseClassification verifies that isolated facts with no
// graph connections to others go to noise when minCommunitySize > 1.
func TestClusterFacts_NoiseClassification(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-noise"
	idx, gs := openGraphTestStore(t, branch)

	// Connected pair: shared entities.
	writeAndSync(t, idx, gs, branch, "kb/pair1.md",
		makeFact("Pair 1", []string{"eng"}, []string{"SharedEntity"}))
	writeAndSync(t, idx, gs, branch, "kb/pair2.md",
		makeFact("Pair 2", []string{"eng"}, []string{"SharedEntity"}))

	// With minCommunitySize=2: the pair should be a cluster.
	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	assertSameCluster(t, result, []string{"kb/pair1.md", "kb/pair2.md"}, "connected pair")

	// With minCommunitySize=100: everything should be noise.
	result2, err := idx.ClusterFacts(ctx, branch, 1.0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Clusters) != 0 {
		t.Errorf("with minCommunitySize=100, expected no clusters, got %v", result2.Clusters)
	}
	noiseSet := map[string]bool{}
	for _, p := range result2.Noise {
		noiseSet[p] = true
	}
	if !noiseSet["kb/pair1.md"] || !noiseSet["kb/pair2.md"] {
		t.Errorf("with minCommunitySize=100, all should be noise, got noise=%v", result2.Noise)
	}
}

// TestClusterFacts_DerivedFromEdges verifies that DERIVED_FROM edges between
// facts influence clustering.
func TestClusterFacts_DerivedFromEdges(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-derivation"
	idx, gs := openGraphTestStore(t, branch)

	// Chain: a ← b (b refs a). Unique entities and domains so only DERIVED_FROM
	// and shared OntologyNode (kb/) connect them.
	writeAndSync(t, idx, gs, branch, "kb/base.md",
		makeFact("Base", []string{"domain-x"}, []string{"EntityX"}))
	writeAndSync(t, idx, gs, branch, "kb/derived.md",
		makeFact("Derived", []string{"domain-y"}, []string{"EntityY"}, "kb/base.md"))

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Both should appear in results (either clustered or noise).
	all := clusterPaths(result)
	if !all["kb/base.md"] || !all["kb/derived.md"] {
		t.Errorf("expected both facts in results, got %v", all)
	}

	// They should be in the same cluster via DERIVED_FROM + shared OntologyNode.
	assertSameCluster(t, result, []string{"kb/base.md", "kb/derived.md"}, "derived chain")
}

// TestClusterFacts_OntologyHierarchy verifies that facts in the same directory
// share OntologyNode connections.
func TestClusterFacts_OntologyHierarchy(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-ontology"
	idx, gs := openGraphTestStore(t, branch)

	// Same directory, unique entities and domains.
	writeAndSync(t, idx, gs, branch, "kb/project/design.md",
		makeFact("Design", []string{"planning"}, []string{"DesignEnt"}))
	writeAndSync(t, idx, gs, branch, "kb/project/impl.md",
		makeFact("Impl", []string{"coding"}, []string{"ImplEnt"}))

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Should cluster via shared OntologyNode kb/project.
	assertSameCluster(t, result, []string{"kb/project/design.md", "kb/project/impl.md"}, "same directory")
}

// TestClusterFacts_MultiDomainBridge verifies that a fact belonging to multiple
// domains connects the communities from those domains.
func TestClusterFacts_MultiDomainBridge(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-bridge"
	idx, gs := openGraphTestStore(t, branch)

	// Backend group with shared entity.
	writeAndSync(t, idx, gs, branch, "kb/back1.md",
		makeFact("Backend 1", []string{"eng/backend"}, []string{"BackTeam"}))
	writeAndSync(t, idx, gs, branch, "kb/back2.md",
		makeFact("Backend 2", []string{"eng/backend"}, []string{"BackTeam"}))

	// Frontend group with shared entity.
	writeAndSync(t, idx, gs, branch, "kb/front1.md",
		makeFact("Frontend 1", []string{"eng/frontend"}, []string{"FrontTeam"}))
	writeAndSync(t, idx, gs, branch, "kb/front2.md",
		makeFact("Frontend 2", []string{"eng/frontend"}, []string{"FrontTeam"}))

	// Bridge: belongs to both domains AND shares entities with both.
	writeAndSync(t, idx, gs, branch, "kb/bridge.md",
		makeFact("Bridge", []string{"eng/backend", "eng/frontend"}, []string{"BackTeam", "FrontTeam"}))

	// Use minCommunitySize=1 so bridge doesn't get filtered as noise even
	// if Louvain places it in its own singleton community.
	result, err := idx.ClusterFacts(ctx, branch, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}

	// All five should appear in results.
	all := clusterPaths(result)
	for _, p := range []string{"kb/back1.md", "kb/back2.md", "kb/front1.md", "kb/front2.md", "kb/bridge.md"} {
		if !all[p] {
			t.Errorf("expected %s in results, got %v", p, all)
		}
	}

	// Backend pair should cluster together.
	assertSameCluster(t, result, []string{"kb/back1.md", "kb/back2.md"}, "backend pair")
	// Frontend pair should cluster together.
	assertSameCluster(t, result, []string{"kb/front1.md", "kb/front2.md"}, "frontend pair")
}

// TestClusterFacts_EmptyBranch verifies that clustering an empty branch returns
// empty results without errors.
func TestClusterFacts_EmptyBranch(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	branch := "agent/cluster-empty"
	idx.EnsureBranch(ctx, branch, "refs/heads/"+branch)

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Clusters) != 0 {
		t.Errorf("expected no clusters for empty branch, got %v", result.Clusters)
	}
}

// TestClusterFacts_AllNoise verifies that when all facts have unique entities
// and unique domains, and minCommunitySize is high, everything goes to noise.
func TestClusterFacts_AllNoise(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-allnoise"
	idx, gs := openGraphTestStore(t, branch)

	// Each fact has unique entities and unique leaf domain.
	writeAndSync(t, idx, gs, branch, "kb/iso1.md",
		makeFact("Iso 1", []string{"unique-domain-1"}, []string{"Unique1"}))
	writeAndSync(t, idx, gs, branch, "kb/iso2.md",
		makeFact("Iso 2", []string{"unique-domain-2"}, []string{"Unique2"}))
	writeAndSync(t, idx, gs, branch, "kb/iso3.md",
		makeFact("Iso 3", []string{"unique-domain-3"}, []string{"Unique3"}))

	// With a high minCommunitySize, all should be noise.
	result, err := idx.ClusterFacts(ctx, branch, 1.0, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Clusters) != 0 {
		t.Errorf("expected no clusters with minCommunitySize=10, got %v", result.Clusters)
	}
	noiseSet := map[string]bool{}
	for _, p := range result.Noise {
		noiseSet[p] = true
	}
	for _, p := range []string{"kb/iso1.md", "kb/iso2.md", "kb/iso3.md"} {
		if !noiseSet[p] {
			t.Errorf("%s should be in noise, noise=%v", p, result.Noise)
		}
	}
}

// TestClusterFacts_MixedSignals verifies clustering when facts share both
// entities and domains — they should cluster together.
func TestClusterFacts_MixedSignals(t *testing.T) {
	ctx := context.Background()
	branch := "agent/cluster-mixed"
	idx, gs := openGraphTestStore(t, branch)

	// Tight group: same leaf domain + same entities.
	writeAndSync(t, idx, gs, branch, "kb/tight1.md",
		makeFact("Go Handler", []string{"eng/backend/api"}, []string{"Go", "HTTP"}))
	writeAndSync(t, idx, gs, branch, "kb/tight2.md",
		makeFact("Go Router", []string{"eng/backend/api"}, []string{"Go", "HTTP"}))

	result, err := idx.ClusterFacts(ctx, branch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	assertSameCluster(t, result, []string{"kb/tight1.md", "kb/tight2.md"},
		"tight pair (shared domain+entities)")
}

// TestClusterFacts_BranchIsolation verifies that facts on different branches
// don't leak into each other's cluster results.
func TestClusterFacts_BranchIsolation(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	branchA := "agent/iso-a"
	branchB := "agent/iso-b"

	// Shared entity on both branches, but different facts.
	idx.Upsert(ctx, branchA, "c1", FactRecord{
		Path: "kb/a1.md", BlobHash: "bh_a1", Title: "A1",
		Domain: []string{"eng"}, Entities: []string{"Shared"},
	})
	idx.Upsert(ctx, branchA, "c1", FactRecord{
		Path: "kb/a2.md", BlobHash: "bh_a2", Title: "A2",
		Domain: []string{"eng"}, Entities: []string{"Shared"},
	})

	idx.Upsert(ctx, branchB, "c2", FactRecord{
		Path: "kb/b1.md", BlobHash: "bh_b1", Title: "B1",
		Domain: []string{"eng"}, Entities: []string{"Shared"},
	})
	idx.Upsert(ctx, branchB, "c2", FactRecord{
		Path: "kb/b2.md", BlobHash: "bh_b2", Title: "B2",
		Domain: []string{"eng"}, Entities: []string{"Shared"},
	})

	// BranchA clusters should not include branchB facts.
	resultA, err := idx.ClusterFacts(ctx, branchA, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pathsA := clusterPaths(resultA)
	if pathsA["kb/b1.md"] || pathsA["kb/b2.md"] {
		t.Errorf("branchA should not contain branchB facts, got %v", pathsA)
	}
	if !pathsA["kb/a1.md"] || !pathsA["kb/a2.md"] {
		t.Errorf("branchA should contain its own facts, got %v", pathsA)
	}

	// BranchB clusters should not include branchA facts.
	resultB, err := idx.ClusterFacts(ctx, branchB, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pathsB := clusterPaths(resultB)
	if pathsB["kb/a1.md"] || pathsB["kb/a2.md"] {
		t.Errorf("branchB should not contain branchA facts, got %v", pathsB)
	}
	if !pathsB["kb/b1.md"] || !pathsB["kb/b2.md"] {
		t.Errorf("branchB should contain its own facts, got %v", pathsB)
	}
}

// --- helpers ---

func pathf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

// assertSameCluster checks that all paths in `group` appear in the same cluster.
func assertSameCluster(t *testing.T, result ClusterResult, group []string, label string) {
	t.Helper()
	if len(group) == 0 {
		return
	}
	first := group[0]
	var clusterMembers []string
	for _, members := range result.Clusters {
		for _, p := range members {
			if p == first {
				clusterMembers = members
				break
			}
		}
		if clusterMembers != nil {
			break
		}
	}
	if clusterMembers == nil {
		t.Errorf("%s: %s not found in any cluster, clusters=%v noise=%v", label, first, result.Clusters, result.Noise)
		return
	}
	members := map[string]bool{}
	for _, p := range clusterMembers {
		members[p] = true
	}
	for _, p := range group {
		if !members[p] {
			t.Errorf("%s: %s not in same cluster as %s, cluster=%v", label, p, first, clusterMembers)
		}
	}
}

// findClusterContaining returns the cluster members that contain path, or nil.
func findClusterContaining(result ClusterResult, path string) []string {
	for _, members := range result.Clusters {
		for _, p := range members {
			if p == path {
				return members
			}
		}
	}
	return nil
}
