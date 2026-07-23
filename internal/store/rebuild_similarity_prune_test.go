package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Rebuild recomputes the similarity layer in full from facts_vec, so it must
// REPLACE it rather than add to it. Before the type-scoped wipe, Rebuild only
// ever merged: stale edges from an earlier corpus state survived alongside the
// fresh top-K, out-degree drifted past knnK, and the inflated graph fed Louvain.
// Measured on a real upgraded corpus: 7,445 -> 9,792 SIMILAR_TO edges and max
// out-degree 28 -> 34, against a knnK of 10.
//
// Superseded fact versions are why the wipe has to be global. Their nodes keep
// their outgoing edges forever (the incremental path only rewrites the *new*
// version's), and the cohesion reader anchors by path rather than by version,
// so those edges surface in reads. Pruning per current-fact source never
// reaches them.
func TestRebuildGraph_PrunesStaleSimilarityEdges(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	// Without an embedder the similarity phase is skipped entirely and every
	// assertion below would pass vacuously on an empty edge set.
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	branch := "main"

	for _, f := range []struct{ path, title string }{
		{"kb/a.md", "alpha"}, {"kb/b.md", "beta"}, {"kb/c.md", "gamma"},
	} {
		_, err = svc.Facts().WriteFact(ctx, branch, f.path,
			testFactBody(f.title, 0.9, nil), "init "+f.path, "")
		require.NoError(t, err)
	}

	db := svc.rh.db
	countSimilar := func() int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeSimilarTo).Scan(&n))
		return n
	}
	countDerived := func() int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&n))
		return n
	}

	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))
	baseSimilar, baseDerived := countSimilar(), countDerived()

	// A stale edge between two orphan nodes: no `facts` row refers to them, so
	// no per-source prune driven by the current fact set would ever visit them.
	// This is the shape a superseded fact version leaves behind.
	ghostSrc, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/gone.md", "blob_hash": "staleblob1"})
	require.NoError(t, err)
	ghostTgt, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/gone2.md", "blob_hash": "staleblob2"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, ghostSrc, ghostTgt, EdgeSimilarTo))
	require.Equal(t, baseSimilar+1, countSimilar(), "stale edge seeded")

	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))

	require.Equal(t, baseSimilar, countSimilar(),
		"Rebuild must replace the similarity layer, dropping edges no longer in any top-K")
	var ghostLeft int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ? AND source_id = ?`,
		EdgeSimilarTo, ghostSrc).Scan(&ghostLeft))
	require.Zero(t, ghostLeft, "the stale edge specifically must be gone")

	require.Equal(t, baseDerived, countDerived(),
		"DERIVED_FROM is an immutable temporal assertion and must NOT be pruned")
}

// Out-degree <= knnK holds by construction once the layer is replaced rather
// than accumulated: each source contributes at most K merges per rebuild.
// (In-degree legitimately exceeds K — popular facts are neighbours of many.)
func TestRebuildGraph_SimilarityRespectsTopKCap(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	// Without an embedder the similarity phase is skipped entirely and every
	// assertion below would pass vacuously on an empty edge set.
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	branch := "main"
	for i, title := range []string{"one", "two", "three", "four", "five"} {
		_, err = svc.Facts().WriteFact(ctx, branch,
			"kb/f"+string(rune('a'+i))+".md",
			testFactBody("similar content "+title, 0.9, nil), "init", "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))

	var maxOut int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(c), 0) FROM (
			SELECT COUNT(*) c FROM edges WHERE type = ? GROUP BY source_id)`,
		EdgeSimilarTo).Scan(&maxOut))
	require.LessOrEqual(t, maxOut, knnK, "no source may exceed the top-K cap")
}

// A second Rebuild must be a no-op on the similarity layer. Wipe-then-remerge
// makes this exact rather than incidental.
func TestRebuildGraph_SimilarityIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	// Without an embedder the similarity phase is skipped entirely and every
	// assertion below would pass vacuously on an empty edge set.
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	branch := "main"
	for _, p := range []string{"kb/x.md", "kb/y.md"} {
		_, err = svc.Facts().WriteFact(ctx, branch, p,
			testFactBody("shared subject matter", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}

	pairs := func() []string {
		rows, err := svc.rh.db.QueryContext(ctx,
			`SELECT source_id || '->' || target_id FROM edges WHERE type = ? ORDER BY 1`,
			EdgeSimilarTo)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			out = append(out, s)
		}
		return out
	}

	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))
	first := pairs()
	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))
	require.Equal(t, first, pairs(), "repeated Rebuild must yield an identical similarity set")
}

// Rebuild must leave planner statistics behind. Without them SQLite plans the
// graph reads blind and drives the anchor-driven SIMILAR_TO query off
// idx_edges_type — a near-scan per anchor, measured at 1722ms vs 11ms on a
// 23.9k-edge corpus. The only other ANALYZE is at DB open, which on a fresh
// clone runs before these tables have any rows.
func TestRebuild_LeavesPlannerStatistics(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	// Without an embedder the similarity phase is skipped entirely and every
	// assertion below would pass vacuously on an empty edge set.
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	graphStats := func() int {
		var n int
		require.NoError(t, svc.rh.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT tbl) FROM sqlite_stat1
			 WHERE tbl IN ('edges','nodes','node_props_text','node_labels')`).Scan(&n))
		return n
	}

	for _, p := range []string{"kb/s1.md", "kb/s2.md", "kb/s3.md"} {
		_, err = svc.Facts().WriteFact(ctx, "main", p,
			testFactBody("statistics fixture", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}
	// Incremental path: Sync's optimize keeps stats current as the corpus grows.
	require.NotZero(t, graphStats(), "Sync must leave planner stats on the graph tables")

	// Isolate Rebuild's own ANALYZE: drop what Sync left, so passing below can
	// only come from Rebuild. This is the fresh-clone state — graph tables
	// populated after the last time anything analysed them.
	_, err = svc.rh.db.ExecContext(ctx, `DELETE FROM sqlite_stat1`)
	require.NoError(t, err)
	require.Zero(t, graphStats(), "stats cleared")

	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))
	require.NotZero(t, graphStats(),
		"Rebuild must ANALYZE the graph tables, else query plans regress badly")
}
