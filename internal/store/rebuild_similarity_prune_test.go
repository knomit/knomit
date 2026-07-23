package store

import (
	"context"
	"fmt"
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

	// The later facts cite the earlier ones, so the rebuild has real
	// DERIVED_FROM edges to preserve. Without refs there are none, and the
	// "must NOT be pruned" assertion below compares 0 against 0.
	for _, f := range []struct {
		path, title string
		refs        []string
	}{
		{"kb/a.md", "alpha", nil},
		{"kb/b.md", "beta", []string{"kb/a.md"}},
		{"kb/c.md", "gamma", []string{"kb/a.md", "kb/b.md"}},
	} {
		_, err = svc.Facts().WriteFact(ctx, branch, f.path,
			testFactBody(f.title, 0.9, f.refs), "init "+f.path, "")
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
	// An empty layer satisfies every "must be gone" assertion below. Pin the
	// floor so a threshold or embedder change cannot silently gut this.
	require.Greater(t, baseSimilar, 0, "three mutually similar facts must produce edges")
	require.Greater(t, baseDerived, 0, "the DERIVED_FROM assertions must exist to be preserved")

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

// Bounded out-degree holds only because the layer is REPLACED rather than
// accumulated: each source contributes at most one KNN pass worth of merges per
// rebuild, but a source that kept edges from an earlier corpus state drifts
// past the bound. (In-degree legitimately exceeds it — popular facts are
// neighbours of many.)
//
// The bound is knnK+1, not knnK. The KNN asks sqlite-vec for knnK+1 rows so the
// fact's own row can be filtered out afterwards and still leave K neighbours;
// when ties push that self-row outside the returned set, all knnK+1 rows are
// neighbours and the source keeps them all. Asserting <= knnK instead would be
// asserting an invariant the implementation does not have — it only looks true
// on a corpus smaller than the bound, where no source has enough candidates to
// reach it.
//
// Two things are needed for this to be able to fail at all, and both were
// missing when it was first written. The corpus must exceed the bound, or it
// sits above the maximum achievable out-degree and no behaviour can breach it.
// And a surplus has to be present BEFORE the rebuild under test, or there is
// nothing for a merge-only pass to carry forward.
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
	db := svc.rh.db

	// More facts than the bound, so every source has more candidates than it
	// can keep and the truncation is real rather than incidental.
	const maxOutDegree = knnK + 1
	const corpus = maxOutDegree + 3
	for i := range corpus {
		_, err = svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/f%02d.md", i),
			testFactBody(fmt.Sprintf("similar content %02d", i), 0.9, nil), "init", "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))

	maxOut := func() int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(c), 0) FROM (
				SELECT COUNT(*) c FROM edges WHERE type = ? GROUP BY source_id)`,
			EdgeSimilarTo).Scan(&n))
		return n
	}
	require.Equal(t, maxOutDegree, maxOut(),
		"the bound must bind exactly here: below it the corpus is too small for this "+
			"test to prove anything, above it the rebuild is already accumulating")

	// Give one source every other fact as a neighbour — the shape a corpus that
	// has churned leaves behind, where edges outlive the top-K that produced
	// them. Targets are live fact nodes, so orphan GC cannot clear them and the
	// prune is the only thing that can.
	var busiest int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT source_id FROM edges WHERE type = ?
		GROUP BY source_id ORDER BY COUNT(*) DESC LIMIT 1`,
		EdgeSimilarTo).Scan(&busiest))

	rows, err := db.QueryContext(ctx,
		`SELECT node_id FROM node_labels WHERE label = ? AND node_id <> ?`, NodeFact, busiest)
	require.NoError(t, err)
	var targets []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		targets = append(targets, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, targets, corpus-1)
	for _, tgt := range targets {
		require.NoError(t, graphMergeEdge(ctx, db, busiest, tgt, EdgeSimilarTo))
	}
	require.Greater(t, maxOut(), maxOutDegree, "surplus seeded")

	require.NoError(t, svc.si.Rebuild(ctx, branch, nil))

	require.Equal(t, maxOutDegree, maxOut(),
		"no source may exceed the KNN bound — a merge-only rebuild carries the surplus forward")
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
	require.NotEmpty(t, first,
		"two mutually similar facts must produce edges — on an empty layer this test is vacuous")
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
