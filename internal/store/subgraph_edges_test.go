package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// mergeFactNode creates a bare Fact node with the given path/blob_hash.
func mergeFactNode(t *testing.T, si *searchIndex, path, bh string) int64 {
	t.Helper()
	_, err := si.rh.db.Exec(`SELECT cypher('MERGE (f:Fact {path: "` + path + `", blob_hash: "` + bh + `"})')`)
	require.NoError(t, err)
	// graphSyncFact always sets deleted=false on live nodes; mirror that so the
	// "NOT deleted = true" filter (which treats an absent property as null) sees
	// these fixtures as live, exactly like production-indexed facts.
	_, err = si.rh.db.Exec(`SELECT cypher('MATCH (f:Fact {path: "` + path + `"}) SET f.deleted = false')`)
	require.NoError(t, err)
	id, err := si.graphNodeIDByProp(context.Background(), NodeFact, "path", path)
	require.NoError(t, err)
	require.NotZero(t, id)
	return id
}

// normalizePairs renders undirected edges as a stable set of "x|y" keys with
// each pair sorted, so assertions don't depend on direction or ordering.
func normalizePairs(pairs [][2]string) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		a, b := p[0], p[1]
		if a > b {
			a, b = b, a
		}
		out = append(out, a+"|"+b)
	}
	sort.Strings(out)
	return out
}

// TestSubgraphEdges_ReturnsSimilarToAmongPaths verifies SubgraphEdges returns
// exactly the SIMILAR_TO edges whose BOTH endpoints are in the requested path
// set, as undirected pairs — excluding edges that leave the set and isolated
// nodes.
func TestSubgraphEdges_ReturnsSimilarToAmongPaths(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	si := svc.Search().(*searchIndex)
	ctx := context.Background()

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")
	c := mergeFactNode(t, si, "kb/c.md", "cccc")
	mergeFactNode(t, si, "kb/lonely.md", "dddd") // in set but no edges
	outside := mergeFactNode(t, si, "kb/outside.md", "eeee")

	// SIMILAR_TO: a-b and b-c inside the set; c-outside leaves the set.
	require.NoError(t, si.graphInsertEdge(ctx, a, b, EdgeSimilarTo))
	require.NoError(t, si.graphInsertEdge(ctx, b, c, EdgeSimilarTo))
	require.NoError(t, si.graphInsertEdge(ctx, c, outside, EdgeSimilarTo))

	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/lonely.md"}
	edges, err := si.SubgraphEdges(ctx, paths)
	require.NoError(t, err)

	got := normalizePairs(edges)
	want := []string{"kb/a.md|kb/b.md", "kb/b.md|kb/c.md"}
	require.Equal(t, want, got, "should return only SIMILAR_TO edges fully inside the path set")
}

// TestSubgraphEdges_ChunksLargePathSets verifies that a path set larger than
// the per-query chunk size still returns every intra-set edge, with cross-chunk
// edges found and deduped. This regresses the "Expression tree is too large"
// failure: a single OR-chain over hundreds of paths blew SQLite's expression
// depth limit, so the query must be split into bounded chunks.
func TestSubgraphEdges_ChunksLargePathSets(t *testing.T) {
	orig := subgraphEdgeChunkSize
	subgraphEdgeChunkSize = 2
	defer func() { subgraphEdgeChunkSize = orig }()

	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	si := svc.Search().(*searchIndex)
	ctx := context.Background()

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")
	c := mergeFactNode(t, si, "kb/c.md", "cccc")
	d := mergeFactNode(t, si, "kb/d.md", "dddd")

	// A chain a–b–c–d; with chunk size 2 the path set [a,b,c,d] spans two
	// chunks, and b–c is the cross-chunk edge.
	require.NoError(t, si.graphInsertEdge(ctx, a, b, EdgeSimilarTo))
	require.NoError(t, si.graphInsertEdge(ctx, b, c, EdgeSimilarTo))
	require.NoError(t, si.graphInsertEdge(ctx, c, d, EdgeSimilarTo))

	edges, err := si.SubgraphEdges(ctx, []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md"})
	require.NoError(t, err)

	got := normalizePairs(edges)
	want := []string{"kb/a.md|kb/b.md", "kb/b.md|kb/c.md", "kb/c.md|kb/d.md"}
	require.Equal(t, want, got, "every intra-set edge must survive chunking, including the cross-chunk one")
}

// TestSubgraphEdges_ExcludesDeletedNodes verifies a soft-deleted endpoint drops
// its edges so clustering never groups facts no longer present.
func TestSubgraphEdges_ExcludesDeletedNodes(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	si := svc.Search().(*searchIndex)
	ctx := context.Background()

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")
	require.NoError(t, si.graphInsertEdge(ctx, a, b, EdgeSimilarTo))

	// Soft-delete b.
	_, err = si.rh.db.Exec(`SELECT cypher('MATCH (f:Fact {path: "kb/b.md"}) SET f.deleted = true')`)
	require.NoError(t, err)

	edges, err := si.SubgraphEdges(ctx, []string{"kb/a.md", "kb/b.md"})
	require.NoError(t, err)
	require.Empty(t, normalizePairs(edges), "edges touching a deleted node must be excluded")
}
