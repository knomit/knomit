package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"knomit/internal/git"
)

// makeFact builds a minimal valid fact markdown with the given title, entities, and refs.
func makeFact(title string, domain []string, entities []string, refs ...string) string {
	domainYAML := "domain: ["
	for i, d := range domain {
		if i > 0 {
			domainYAML += ", "
		}
		domainYAML += d
	}
	domainYAML += "]"

	entYAML := "entities: ["
	for i, e := range entities {
		if i > 0 {
			entYAML += ", "
		}
		entYAML += e
	}
	entYAML += "]"

	refsYAML := "refs: []"
	if len(refs) > 0 {
		refsYAML = "refs:\n"
		for _, r := range refs {
			refsYAML += fmt.Sprintf("  - %s\n", r)
		}
	}

	return fmt.Sprintf("---\ntype: observation\n%s\nconfidence: 0.9\nsources: 1\n%s\n%s\n---\n# %s\n\nBody of %s.\n",
		domainYAML, entYAML, refsYAML, title, title)
}

// openGraphTestStore creates a Service + GitStore for graph tests using the full
// write pipeline. Writes and syncs each file individually in order so that
// DERIVED_FROM targets exist before referencing facts are indexed.
func openGraphTestStore(t *testing.T, branch string) (*Index, *git.Store) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, branch)
	if err != nil {
		t.Fatal(err)
	}

	return svc.Index(), gs
}

// writeAndSync writes a single file via the git store then syncs the index.
// This ensures DERIVED_FROM targets exist before referencing facts are indexed.
func writeAndSync(t *testing.T, idx *Index, gs *git.Store, branch, path, content string) {
	ctx := context.Background()
	t.Helper()
	if _, _, err := gs.WriteFile(branch, path, content, "add "+path, "learn"); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if err := idx.Sync(ctx, gs, branch); err != nil {
		t.Fatalf("Sync after %s: %v", path, err)
	}
}

// TestGraphRelationships_SameBranch verifies that multiple facts on the same branch
// that reference each other produce correct DERIVED_FROM edges, and that ExplainFact
// returns the relationships.
func TestGraphRelationships_SameBranch(t *testing.T) {
	ctx := context.Background()
	branch := "agent/history-test"
	idx, gs := openGraphTestStore(t, branch)

	// Write in dependency order: targets first, then referencing facts.
	writeAndSync(t, idx, gs, branch, "kb/arch.md",
		makeFact("Architecture", []string{"eng/software"}, []string{"System"}))
	writeAndSync(t, idx, gs, branch, "kb/api.md",
		makeFact("API Design", []string{"eng/software"}, []string{"System", "REST"}, "kb/arch.md"))
	writeAndSync(t, idx, gs, branch, "kb/db.md",
		makeFact("Database Schema", []string{"eng/software"}, []string{"System", "PostgreSQL"}, "kb/arch.md", "kb/api.md"))

	// Verify DERIVED_FROM edges.
	got := derivedFromPaths(t, idx, "kb/db.md", blobHash(t, idx, branch, "kb/db.md"))
	if !got["kb/arch.md"] || !got["kb/api.md"] || len(got) != 2 {
		t.Errorf("db DERIVED_FROM: want {arch, api}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/api.md", blobHash(t, idx, branch, "kb/api.md"))
	if !got["kb/arch.md"] || len(got) != 1 {
		t.Errorf("api DERIVED_FROM: want {arch}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/arch.md", blobHash(t, idx, branch, "kb/arch.md"))
	if len(got) != 0 {
		t.Errorf("arch DERIVED_FROM: want 0 edges, got %v", got)
	}

	// Verify via ExplainFact.
	res, err := idx.ExplainFact(ctx, branch, "kb/arch.md")
	if err != nil {
		t.Fatal(err)
	}
	incomingPaths := map[string]bool{}
	for _, r := range res.Incoming {
		incomingPaths[r.Path] = true
	}
	if !incomingPaths["kb/api.md"] || !incomingPaths["kb/db.md"] {
		t.Errorf("arch incoming: want {api, db}, got %v", incomingPaths)
	}
}

// TestGraphRelationships_FactChanges verifies that when facts change (new blob_hash),
// old graph nodes retain their edges (immutable per-version), and the new version
// gets its own edges.
func TestGraphRelationships_FactChanges(t *testing.T) {
	ctx := context.Background()
	branch := "agent/history-change"
	idx, gs := openGraphTestStore(t, branch)

	// Phase 1: arch, db (no refs), then api → arch.
	writeAndSync(t, idx, gs, branch, "kb/arch.md",
		makeFact("Architecture v1", []string{"eng"}, []string{"System"}))
	writeAndSync(t, idx, gs, branch, "kb/db.md",
		makeFact("Database v1", []string{"eng"}, []string{"PostgreSQL"}))
	writeAndSync(t, idx, gs, branch, "kb/api.md",
		makeFact("API v1", []string{"eng"}, []string{"REST"}, "kb/arch.md"))

	apiV1Hash := blobHash(t, idx, branch, "kb/api.md")
	got := derivedFromPaths(t, idx, "kb/api.md", apiV1Hash)
	if !got["kb/arch.md"] || len(got) != 1 {
		t.Fatalf("api v1 DERIVED_FROM: want {arch}, got %v", got)
	}

	// Phase 2: api changes — now refs db instead of arch.
	writeAndSync(t, idx, gs, branch, "kb/api.md",
		makeFact("API v2", []string{"eng"}, []string{"REST", "gRPC"}, "kb/db.md"))

	apiV2Hash := blobHash(t, idx, branch, "kb/api.md")
	if apiV1Hash == apiV2Hash {
		t.Fatal("expected different blob hashes for v1 and v2")
	}

	// v1 edges are immutable — still point to arch.
	got = derivedFromPaths(t, idx, "kb/api.md", apiV1Hash)
	if !got["kb/arch.md"] || len(got) != 1 {
		t.Errorf("api v1 DERIVED_FROM after update: want {arch}, got %v", got)
	}

	// v2 edges point to db.
	got = derivedFromPaths(t, idx, "kb/api.md", apiV2Hash)
	if !got["kb/db.md"] || len(got) != 1 {
		t.Errorf("api v2 DERIVED_FROM: want {db}, got %v", got)
	}

	// v1 entity was REST, v2 has REST + gRPC.
	v1entities := taggedEntities(t, idx, "kb/api.md", apiV1Hash)
	if !v1entities["REST"] || v1entities["gRPC"] {
		t.Errorf("api v1 entities: want {REST}, got %v", v1entities)
	}
	v2entities := taggedEntities(t, idx, "kb/api.md", apiV2Hash)
	if !v2entities["REST"] || !v2entities["gRPC"] {
		t.Errorf("api v2 entities: want {REST, gRPC}, got %v", v2entities)
	}

	// After GC, v1 is orphaned (replaced by v2 on this branch).
	if err := idx.GC(ctx); err != nil {
		t.Fatal(err)
	}
	got = derivedFromPaths(t, idx, "kb/api.md", apiV2Hash)
	if !got["kb/db.md"] {
		t.Errorf("api v2 DERIVED_FROM after GC: want {db}, got %v", got)
	}
}

// TestGraphRelationships_BranchDivergence verifies that facts diverging across
// branches maintain independent graph relationships and that branch-scoped queries
// return the correct view.
//
// Uses Upsert directly for multi-branch scenarios since git branch forking is
// outside the scope of graph tests.
func TestGraphRelationships_BranchDivergence(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	root := "agent/root"
	branchA := "agent/branch-a"
	branchB := "agent/branch-b"

	// Shared base on root: arch + api → arch.
	idx.Upsert(ctx, root, "c0", FactRecord{
		Path: "kb/arch.md", BlobHash: "bh_arch", Title: "Architecture",
		Domain: []string{"eng"}, Entities: []string{"System"},
	})
	idx.Upsert(ctx, root, "c0", FactRecord{
		Path: "kb/api.md", BlobHash: "bh_api", Title: "API",
		Domain: []string{"eng"}, Entities: []string{"System", "REST"},
		Refs: []string{"kb/arch.md"},
	})

	// Fork via MergeBranch (COW — same fact rows, new branch_facts pointers).
	idx.MergeBranch(ctx, root, branchA)
	idx.MergeBranch(ctx, root, branchB)

	// BranchA: add a db fact referencing api.
	idx.Upsert(ctx, branchA, "cA1", FactRecord{
		Path: "kb/db.md", BlobHash: "bh_db_a", Title: "DB (branch A)",
		Domain: []string{"eng"}, Entities: []string{"PostgreSQL"},
		Refs: []string{"kb/api.md"},
	})

	// BranchB: modify api to drop the arch ref, add a new fact.
	idx.Upsert(ctx, branchB, "cB1", FactRecord{
		Path: "kb/api.md", BlobHash: "bh_api_b", Title: "API (branch B)",
		Domain: []string{"eng"}, Entities: []string{"System", "gRPC"},
	})
	idx.Upsert(ctx, branchB, "cB1", FactRecord{
		Path: "kb/cache.md", BlobHash: "bh_cache_b", Title: "Cache (branch B)",
		Domain: []string{"eng"}, Entities: []string{"Redis"},
		Refs: []string{"kb/api.md"},
	})

	// --- Verify branch-scoped graph visibility via ClusterFacts ---

	resultA, err := idx.ClusterFacts(ctx, branchA, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pathsA := clusterPaths(resultA)
	for _, p := range []string{"kb/arch.md", "kb/api.md", "kb/db.md"} {
		if !pathsA[p] {
			t.Errorf("branchA: expected %s in cluster results, got %v", p, pathsA)
		}
	}
	if pathsA["kb/cache.md"] {
		t.Error("branchA: cache.md (branchB only) should not be visible")
	}

	resultB, err := idx.ClusterFacts(ctx, branchB, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pathsB := clusterPaths(resultB)
	for _, p := range []string{"kb/arch.md", "kb/api.md", "kb/cache.md"} {
		if !pathsB[p] {
			t.Errorf("branchB: expected %s in cluster results, got %v", p, pathsB)
		}
	}
	if pathsB["kb/db.md"] {
		t.Error("branchB: db.md (branchA only) should not be visible")
	}

	// --- Verify graph relationships are version-specific ---

	got := derivedFromPaths(t, idx, "kb/api.md", "bh_api")
	if !got["kb/arch.md"] {
		t.Errorf("branchA api DERIVED_FROM: want {arch}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/api.md", "bh_api_b")
	if len(got) != 0 {
		t.Errorf("branchB api DERIVED_FROM: want empty, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/db.md", "bh_db_a")
	if !got["kb/api.md"] || len(got) != 1 {
		t.Errorf("branchA db DERIVED_FROM: want {api}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/cache.md", "bh_cache_b")
	if !got["kb/api.md"] || len(got) != 1 {
		t.Errorf("branchB cache DERIVED_FROM: want {api}, got %v", got)
	}

	// --- Graph expansion should be branch-scoped ---

	branchAID, _ := idx.branchID(ctx, branchA)
	branchBID, _ := idx.branchID(ctx, branchB)

	expandedA := idx.graphExpandSearch(ctx, branchAID, map[string]float64{"kb/arch.md": 0.9}, 1)
	if _, ok := expandedA["kb/cache.md"]; ok {
		t.Error("branchA expansion: cache.md should not leak from branchB")
	}

	expandedB := idx.graphExpandSearch(ctx, branchBID, map[string]float64{"kb/arch.md": 0.9}, 1)
	if _, ok := expandedB["kb/db.md"]; ok {
		t.Error("branchB expansion: db.md should not leak from branchA")
	}
}

// TestGraphRelationships_COWSharedFacts verifies that COW-shared facts (same path,
// same blob_hash on multiple branches) produce a single graph node with shared edges.
func TestGraphRelationships_COWSharedFacts(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	branchA := "agent/cow-a"
	branchB := "agent/cow-b"

	// Create on branchA, then COW to branchB via MergeBranch.
	idx.Upsert(ctx, branchA, "c1", FactRecord{
		Path: "kb/target.md", BlobHash: "bh_target", Title: "Target",
		Domain: []string{"eng"}, Entities: []string{"Go"},
	})
	idx.Upsert(ctx, branchA, "c1", FactRecord{
		Path: "kb/shared.md", BlobHash: "bh_shared", Title: "Shared",
		Domain: []string{"eng"}, Entities: []string{"Go"},
		Refs: []string{"kb/target.md"},
	})
	idx.MergeBranch(ctx, branchA, branchB)

	// Only ONE Fact node should exist for shared.md (same path + blob_hash).
	var nodeCount int
	idx.db.QueryRow(
		`SELECT count(*) FROM json_each(cypher('MATCH (f:Fact {path: "kb/shared.md"}) RETURN f.path AS path'))`,
	).Scan(&nodeCount)
	if nodeCount != 1 {
		t.Errorf("expected 1 Fact node for COW-shared fact, got %d", nodeCount)
	}

	// DERIVED_FROM edge should exist.
	got := derivedFromPaths(t, idx, "kb/shared.md", "bh_shared")
	if !got["kb/target.md"] {
		t.Errorf("COW shared fact DERIVED_FROM: want {target}, got %v", got)
	}

	// Both branches see the fact.
	for _, b := range []string{branchA, branchB} {
		result, err := idx.ClusterFacts(ctx, b, 1.0, 1)
		if err != nil {
			t.Fatal(err)
		}
		paths := clusterPaths(result)
		if !paths["kb/shared.md"] {
			t.Errorf("branch %s: expected shared.md in clusters", b)
		}
	}
}

// TestGraphRelationships_MergeBranchPreservesEdges verifies that merging branches
// preserves graph relationships: src's fact versions become visible on dst.
func TestGraphRelationships_MergeBranchPreservesEdges(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	src := "agent/feature"
	dst := "agent/main"

	// Base fact on dst.
	idx.Upsert(ctx, dst, "c0", FactRecord{
		Path: "kb/base.md", BlobHash: "bh_base", Title: "Base",
		Domain: []string{"eng"}, Entities: []string{"Core"},
	})

	// Feature branch adds a fact referencing base.
	idx.Upsert(ctx, src, "c1", FactRecord{
		Path: "kb/feature.md", BlobHash: "bh_feature", Title: "Feature",
		Domain: []string{"eng"}, Entities: []string{"Core", "NewThing"},
		Refs: []string{"kb/base.md"},
	})

	// Before merge, dst should NOT see feature.md.
	resultPre, err := idx.ClusterFacts(ctx, dst, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if clusterPaths(resultPre)["kb/feature.md"] {
		t.Error("dst should not see feature.md before merge")
	}

	// Merge src → dst.
	if err := idx.MergeBranch(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	// After merge, dst sees both facts.
	resultPost, err := idx.ClusterFacts(ctx, dst, 1.0, 1)
	if err != nil {
		t.Fatal(err)
	}
	pathsPost := clusterPaths(resultPost)
	if !pathsPost["kb/base.md"] || !pathsPost["kb/feature.md"] {
		t.Errorf("dst after merge: want {base, feature}, got %v", pathsPost)
	}

	// Feature's DERIVED_FROM edge to base is still intact.
	got := derivedFromPaths(t, idx, "kb/feature.md", "bh_feature")
	if !got["kb/base.md"] {
		t.Errorf("feature DERIVED_FROM after merge: want {base}, got %v", got)
	}
}

// TestGraphRelationships_DropBranchCleansOrphans verifies that dropping a branch
// GCs facts that are only referenced by that branch, including their graph nodes.
func TestGraphRelationships_DropBranchCleansOrphans(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	keep := "agent/keep"
	drop := "agent/drop"

	// Shared fact on both branches.
	idx.Upsert(ctx, keep, "c1", FactRecord{
		Path: "kb/shared.md", BlobHash: "bh_shared", Title: "Shared",
		Domain: []string{"eng"}, Entities: []string{"Go"},
	})
	idx.MergeBranch(ctx, keep, drop)

	// Orphan-to-be: only on drop branch.
	idx.Upsert(ctx, drop, "c1", FactRecord{
		Path: "kb/orphan.md", BlobHash: "bh_orphan", Title: "Orphan",
		Domain: []string{"eng"}, Entities: []string{"Temp"},
		Refs: []string{"kb/shared.md"},
	})

	// Verify orphan exists in graph before drop.
	got := derivedFromPaths(t, idx, "kb/orphan.md", "bh_orphan")
	if !got["kb/shared.md"] {
		t.Fatalf("orphan DERIVED_FROM before drop: want {shared}, got %v", got)
	}

	// Drop the branch.
	if err := idx.DropBranch(ctx, drop); err != nil {
		t.Fatal(err)
	}

	// Orphan fact row should be deleted.
	var factCount int
	idx.db.QueryRow(`SELECT count(*) FROM facts WHERE path = 'kb/orphan.md'`).Scan(&factCount)
	if factCount != 0 {
		t.Errorf("orphan fact should be deleted after drop, count=%d", factCount)
	}

	// Orphan graph node should be marked deleted.
	var deleted int
	err = idx.db.QueryRow(
		`SELECT json_extract(value, '$.deleted') FROM json_each(cypher('MATCH (f:Fact {path: "kb/orphan.md"}) WHERE f.blob_hash = "bh_orphan" RETURN f.deleted AS deleted'))`,
	).Scan(&deleted)
	if err != nil {
		// Node may not exist at all after GC, which is also acceptable.
		return
	}
	if deleted != 1 {
		t.Errorf("orphan graph node: expected deleted=1, got %d", deleted)
	}

	// Shared fact should still exist.
	idx.db.QueryRow(`SELECT count(*) FROM facts WHERE path = 'kb/shared.md'`).Scan(&factCount)
	if factCount != 1 {
		t.Errorf("shared fact should survive drop, count=%d", factCount)
	}
}

// TestGraphRelationships_EntitySharing_AcrossVersions verifies that entity nodes
// are shared across fact versions — updating a fact's entities doesn't orphan the
// entity node if another version still uses it.
func TestGraphRelationships_EntitySharing_AcrossVersions(t *testing.T) {
	branch := "agent/entity-test"
	idx, gs := openGraphTestStore(t, branch)

	// v1: tagged with Go and SQLite.
	writeAndSync(t, idx, gs, branch, "kb/tool.md",
		makeFact("Tool v1", []string{"eng"}, []string{"Go", "SQLite"}))

	v1Hash := blobHash(t, idx, branch, "kb/tool.md")

	// v2: tagged with Go and Redis (dropped SQLite, added Redis).
	writeAndSync(t, idx, gs, branch, "kb/tool.md",
		makeFact("Tool v2", []string{"eng"}, []string{"Go", "Redis"}))

	v2Hash := blobHash(t, idx, branch, "kb/tool.md")

	v1ent := taggedEntities(t, idx, "kb/tool.md", v1Hash)
	if !v1ent["Go"] || !v1ent["SQLite"] {
		t.Errorf("v1 entities: want {Go, SQLite}, got %v", v1ent)
	}
	if v1ent["Redis"] {
		t.Errorf("v1 entities: should not have Redis, got %v", v1ent)
	}

	v2ent := taggedEntities(t, idx, "kb/tool.md", v2Hash)
	if !v2ent["Go"] || !v2ent["Redis"] {
		t.Errorf("v2 entities: want {Go, Redis}, got %v", v2ent)
	}
	if v2ent["SQLite"] {
		t.Errorf("v2 entities: should not have SQLite, got %v", v2ent)
	}

	// Entity node "Go" should be shared (one node, two TAGGED edges pointing to it).
	var goEdgeCount int
	idx.db.QueryRow(
		`SELECT count(*) FROM json_each(cypher('MATCH (f:Fact {path: "kb/tool.md"})-[:TAGGED]->(e:Entity {name: "Go"}) RETURN f.blob_hash AS bh'))`,
	).Scan(&goEdgeCount)
	if goEdgeCount != 2 {
		t.Errorf("expected 2 TAGGED edges to Go entity (one per version), got %d", goEdgeCount)
	}
}

// TestGraphRelationships_CircularRefs verifies that circular references between
// facts work correctly when both nodes exist before edges are created.
//
// Note: GraphQLite has a known bug where MATCH on two nodes of the same label
// degenerates into a self-loop when the target doesn't exist yet (see graph.go:222).
// To avoid this, both facts are first created without refs, then re-written with
// refs in a second pass.
func TestGraphRelationships_CircularRefs(t *testing.T) {
	ctx := context.Background()
	branch := "agent/circular"
	idx, gs := openGraphTestStore(t, branch)

	// Phase 1: create both facts without refs (so graph nodes exist).
	writeAndSync(t, idx, gs, branch, "kb/a.md", makeFact("A", []string{"test"}, nil))
	writeAndSync(t, idx, gs, branch, "kb/b.md", makeFact("B", []string{"test"}, nil))

	// Phase 2: re-write with refs (new content = new blob_hash = new graph nodes).
	writeAndSync(t, idx, gs, branch, "kb/a.md", makeFact("A v2", []string{"test"}, nil, "kb/b.md"))
	writeAndSync(t, idx, gs, branch, "kb/b.md", makeFact("B v2", []string{"test"}, nil, "kb/a.md"))

	aV2Hash := blobHash(t, idx, branch, "kb/a.md")
	bV2Hash := blobHash(t, idx, branch, "kb/b.md")

	gotA := derivedFromPaths(t, idx, "kb/a.md", aV2Hash)
	if !gotA["kb/b.md"] {
		t.Errorf("a DERIVED_FROM: want {b}, got %v", gotA)
	}

	gotB := derivedFromPaths(t, idx, "kb/b.md", bV2Hash)
	if !gotB["kb/a.md"] {
		t.Errorf("b DERIVED_FROM: want {a}, got %v", gotB)
	}

	// ExplainFact should show both directions.
	resA, _ := idx.ExplainFact(ctx, branch, "kb/a.md")
	outPaths := map[string]bool{}
	for _, r := range resA.Outgoing {
		outPaths[r.Path] = true
	}
	if !outPaths["kb/b.md"] {
		t.Errorf("a outgoing: want b, got %v", resA.Outgoing)
	}
	inPaths := map[string]bool{}
	for _, r := range resA.Incoming {
		inPaths[r.Path] = true
	}
	if !inPaths["kb/b.md"] {
		t.Errorf("a incoming: want b, got %v", resA.Incoming)
	}
}

// TestGraphRelationships_MergeOverwrite verifies that merging branches where src
// has a newer version of a fact correctly makes the new version visible on dst.
func TestGraphRelationships_MergeOverwrite(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	src := "agent/src"
	dst := "agent/dst"

	// dst has b.md, then a.md → b.md.
	idx.Upsert(ctx, dst, "c1", FactRecord{
		Path: "kb/b.md", BlobHash: "bh_b", Title: "B",
		Domain: []string{"eng"}, Entities: []string{"X"},
	})
	idx.Upsert(ctx, dst, "c1", FactRecord{
		Path: "kb/a.md", BlobHash: "bh_a_old", Title: "A (old)",
		Domain: []string{"eng"}, Entities: []string{"Old"},
		Refs: []string{"kb/b.md"},
	})

	// src has c.md, then a.md → c.md.
	idx.Upsert(ctx, src, "c2", FactRecord{
		Path: "kb/c.md", BlobHash: "bh_c", Title: "C",
		Domain: []string{"eng"}, Entities: []string{"Y"},
	})
	idx.Upsert(ctx, src, "c2", FactRecord{
		Path: "kb/a.md", BlobHash: "bh_a_new", Title: "A (new)",
		Domain: []string{"eng"}, Entities: []string{"New"},
		Refs: []string{"kb/c.md"},
	})

	// Merge src → dst.
	if err := idx.MergeBranch(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	// dst's view of a.md should now be the new version.
	dstID, _ := idx.branchID(ctx, dst)
	var mergedHash string
	err = idx.db.QueryRow(
		`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id WHERE bf.branch_id = ? AND bf.path = ?`,
		dstID, "kb/a.md",
	).Scan(&mergedHash)
	if err != nil {
		t.Fatal(err)
	}
	if mergedHash != "bh_a_new" {
		t.Errorf("after merge, dst a.md should be new version, got blob_hash=%s", mergedHash)
	}

	// The new version's graph edges should point to c.md.
	got := derivedFromPaths(t, idx, "kb/a.md", "bh_a_new")
	if !got["kb/c.md"] || len(got) != 1 {
		t.Errorf("merged a.md DERIVED_FROM: want {c}, got %v", got)
	}

	// The old version's graph edges are immutable (still point to b.md).
	got = derivedFromPaths(t, idx, "kb/a.md", "bh_a_old")
	if !got["kb/b.md"] || len(got) != 1 {
		t.Errorf("old a.md DERIVED_FROM: want {b}, got %v", got)
	}
}

// TestExplainFact_BranchScoped verifies that ExplainFact filters incoming refs
// by branch visibility: a fact on branchB referencing a shared fact should not
// appear in branchA's ExplainFact incoming results.
func TestExplainFact_BranchScoped(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	branchA := "agent/branch-a"
	branchB := "agent/branch-b"

	// Shared target on both branches.
	idx.Upsert(ctx, branchA, "c1", FactRecord{
		Path: "kb/target.md", BlobHash: "bh_target", Title: "Target",
		Domain: []string{"eng"}, Entities: []string{"Go"},
	})
	idx.MergeBranch(ctx, branchA, branchB)

	// branchA: refA → target.
	idx.Upsert(ctx, branchA, "c2", FactRecord{
		Path: "kb/ref-a.md", BlobHash: "bh_ref_a", Title: "Ref A",
		Domain: []string{"eng"}, Refs: []string{"kb/target.md"},
	})

	// branchB: refB → target.
	idx.Upsert(ctx, branchB, "c3", FactRecord{
		Path: "kb/ref-b.md", BlobHash: "bh_ref_b", Title: "Ref B",
		Domain: []string{"eng"}, Refs: []string{"kb/target.md"},
	})

	// ExplainFact on branchA: incoming should show ref-a but NOT ref-b.
	resA, err := idx.ExplainFact(ctx, branchA, "kb/target.md")
	if err != nil {
		t.Fatal(err)
	}
	inA := map[string]bool{}
	for _, r := range resA.Incoming {
		inA[r.Path] = true
	}
	if !inA["kb/ref-a.md"] {
		t.Errorf("branchA ExplainFact: want ref-a in incoming, got %v", inA)
	}
	if inA["kb/ref-b.md"] {
		t.Errorf("branchA ExplainFact: ref-b should not appear (branchB only), got %v", inA)
	}

	// ExplainFact on branchB: incoming should show ref-b but NOT ref-a.
	resB, err := idx.ExplainFact(ctx, branchB, "kb/target.md")
	if err != nil {
		t.Fatal(err)
	}
	inB := map[string]bool{}
	for _, r := range resB.Incoming {
		inB[r.Path] = true
	}
	if !inB["kb/ref-b.md"] {
		t.Errorf("branchB ExplainFact: want ref-b in incoming, got %v", inB)
	}
	if inB["kb/ref-a.md"] {
		t.Errorf("branchB ExplainFact: ref-a should not appear (branchA only), got %v", inB)
	}
}

// TestGraphRelationships_ThreeBranchesSharedAndDiverged verifies a realistic
// scenario: root creates shared knowledge, two branches diverge with independent
// modifications, and the graph correctly reflects each branch's worldview.
func TestGraphRelationships_ThreeBranchesSharedAndDiverged(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	root := "agent/root"
	dev := "agent/dev"
	exp := "agent/experiment"

	// Shared base: 3 interconnected facts (written in dependency order).
	idx.Upsert(ctx, root, "c0", FactRecord{
		Path: "kb/design.md", BlobHash: "bh_design", Title: "Design",
		Domain: []string{"eng/planning"}, Entities: []string{"System", "Architecture"},
	})
	idx.Upsert(ctx, root, "c0", FactRecord{
		Path: "kb/impl.md", BlobHash: "bh_impl", Title: "Implementation",
		Domain: []string{"eng/code"}, Entities: []string{"System", "Go"},
		Refs: []string{"kb/design.md"},
	})
	idx.Upsert(ctx, root, "c0", FactRecord{
		Path: "kb/test.md", BlobHash: "bh_test", Title: "Tests",
		Domain: []string{"eng/quality"}, Entities: []string{"System", "Testing"},
		Refs: []string{"kb/impl.md", "kb/design.md"},
	})

	// Fork branches.
	idx.MergeBranch(ctx, root, dev)
	idx.MergeBranch(ctx, root, exp)

	// Dev branch: add perf.md, update impl to reference it.
	idx.Upsert(ctx, dev, "c_dev1", FactRecord{
		Path: "kb/perf.md", BlobHash: "bh_perf", Title: "Performance",
		Domain: []string{"eng/code"}, Entities: []string{"System", "Benchmark"},
	})
	idx.Upsert(ctx, dev, "c_dev2", FactRecord{
		Path: "kb/impl.md", BlobHash: "bh_impl_v2", Title: "Impl v2",
		Domain: []string{"eng/code"}, Entities: []string{"System", "Go", "Optimization"},
		Refs: []string{"kb/design.md", "kb/perf.md"},
	})

	// Exp branch: remove test.md, add experiment.md.
	idx.Delete(ctx, exp, "kb/test.md")
	idx.Upsert(ctx, exp, "c_exp1", FactRecord{
		Path: "kb/experiment.md", BlobHash: "bh_experiment", Title: "Experiment",
		Domain: []string{"eng/research"}, Entities: []string{"System", "ML"},
		Refs: []string{"kb/design.md"},
	})

	// --- Verify each branch's view ---
	assertBranchFacts(t, idx, root, []string{"kb/design.md", "kb/impl.md", "kb/test.md"})
	assertBranchFacts(t, idx, dev, []string{"kb/design.md", "kb/impl.md", "kb/test.md", "kb/perf.md"})
	assertBranchFacts(t, idx, exp, []string{"kb/design.md", "kb/impl.md", "kb/experiment.md"})

	// --- Verify graph edges per version ---

	got := derivedFromPaths(t, idx, "kb/impl.md", "bh_impl")
	if !got["kb/design.md"] || len(got) != 1 {
		t.Errorf("root impl DERIVED_FROM: want {design}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/impl.md", "bh_impl_v2")
	if !got["kb/design.md"] || !got["kb/perf.md"] || len(got) != 2 {
		t.Errorf("dev impl DERIVED_FROM: want {design, perf}, got %v", got)
	}

	got = derivedFromPaths(t, idx, "kb/experiment.md", "bh_experiment")
	if !got["kb/design.md"] || len(got) != 1 {
		t.Errorf("exp experiment DERIVED_FROM: want {design}, got %v", got)
	}

	// --- Graph expansion scoping ---

	devID, _ := idx.branchID(ctx, dev)
	expID, _ := idx.branchID(ctx, exp)

	expandedDev := idx.graphExpandSearch(ctx, devID, map[string]float64{"kb/design.md": 0.9}, 1)
	if _, ok := expandedDev["kb/experiment.md"]; ok {
		t.Error("dev expansion should not find experiment.md (exp-only)")
	}

	expandedExp := idx.graphExpandSearch(ctx, expID, map[string]float64{"kb/design.md": 0.9}, 1)
	if _, ok := expandedExp["kb/test.md"]; ok {
		t.Error("exp expansion should not find test.md (deleted from exp)")
	}
	if _, ok := expandedExp["kb/perf.md"]; ok {
		t.Error("exp expansion should not find perf.md (dev-only)")
	}
}

// --- helpers ---

// blobHash returns the blob_hash for a fact on a given branch.
func blobHash(t *testing.T, idx *Index, branch, path string) string {
	ctx := context.Background()
	t.Helper()
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		t.Fatalf("BranchID(%s): %v", branch, err)
	}
	var hash string
	err = idx.db.QueryRow(
		`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id WHERE bf.branch_id = ? AND bf.path = ?`,
		branchID, path,
	).Scan(&hash)
	if err != nil {
		t.Fatalf("blobHash(%s, %s): %v", branch, path, err)
	}
	return hash
}

// taggedEntities returns the set of entity names linked via TAGGED edges from
// a specific fact version (path + blobHash).
func taggedEntities(t *testing.T, idx *Index, path, blobHash string) map[string]bool {
	t.Helper()
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.name') FROM json_each(cypher('MATCH (f:Fact {path: "%s"})-[:TAGGED]->(e:Entity) WHERE f.blob_hash = "%s" RETURN e.name AS name'))`,
		escapeCypherKey(path), escapeCypherKey(blobHash),
	)
	rows, err := idx.db.Query(q)
	if err != nil {
		t.Fatalf("taggedEntities: %v", err)
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var name string
		rows.Scan(&name)
		if name != "" {
			result[name] = true
		}
	}
	return result
}

// clusterPaths collects all fact paths from a ClusterResult (clusters + noise).
func clusterPaths(result ClusterResult) map[string]bool {
	paths := map[string]bool{}
	for _, members := range result.Clusters {
		for _, p := range members {
			paths[p] = true
		}
	}
	for _, p := range result.Noise {
		paths[p] = true
	}
	return paths
}

// assertBranchFacts checks that the given branch sees exactly the expected paths.
func assertBranchFacts(t *testing.T, idx *Index, branch string, wantPaths []string) {
	ctx := context.Background()
	t.Helper()
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		t.Fatalf("BranchID(%s): %v", branch, err)
	}
	rows, err := idx.db.Query(
		`SELECT path FROM branch_facts WHERE branch_id = ? ORDER BY path`, branchID,
	)
	if err != nil {
		t.Fatalf("query branch_facts: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var p string
		rows.Scan(&p)
		got[p] = true
	}
	want := map[string]bool{}
	for _, p := range wantPaths {
		want[p] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("branch %s: missing expected fact %s, got %v", branch, p, got)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("branch %s: unexpected fact %s", branch, p)
		}
	}
}
