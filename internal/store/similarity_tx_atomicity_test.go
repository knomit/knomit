package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// openObserver returns a SECOND connection to the same database file.
//
// The writer's own handle would see its uncommitted work, so asserting against
// it proves nothing about isolation. WAL lets this one read the last committed
// snapshot while the write transaction is still open, which is exactly the
// state another process would be in.
func openObserver(t *testing.T, path string) *sql.DB {
	t.Helper()
	obs, err := sql.Open("sqlite3_knomit",
		path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1")
	require.NoError(t, err)
	t.Cleanup(func() { obs.Close() })
	return obs
}

func countSimilarOn(t *testing.T, db *sql.DB, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeSimilarTo).Scan(&n))
	return n
}

// The incremental similarity rewrite is delete-outgoing followed by re-merge.
// Run bare those are two autocommit statements, so the graph is briefly missing
// that version's edges and a concurrent build for the same source can land its
// own delete in between, dropping edges this pass just wrote — the neighbourhood
// stays thinned until the fact is next re-indexed.
//
// Wrapping both in one transaction is what closes that window. This asserts the
// window is closed from OUTSIDE: a second connection must never observe the
// post-delete, pre-merge state. Without the transaction the delete has already
// committed by the time the hook fires, and the observer sees the layer thinned.
func TestGraphBuildSimilarityEdges_RewriteIsInvisibleUntilCommit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	// Without an embedder the similarity phase is skipped and the hook below
	// never fires — the require.True on `fired` is what makes that loud.
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md"} {
		_, err = svc.Facts().WriteFact(ctx, "main", p,
			testFactBody("shared subject matter for knn", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}

	obs := openObserver(t, dbPath)
	baseline := countSimilarOn(t, obs, ctx)
	require.Greater(t, baseline, 0,
		"the similarity layer must be populated, or the observation below is vacuous")

	var seenMidRewrite int
	fired := false
	svc.si.inSimEdgeTx = func() {
		if fired {
			return
		}
		fired = true
		seenMidRewrite = countSimilarOn(t, obs, ctx)
	}
	defer func() { svc.si.inSimEdgeTx = nil }()

	// Re-index one fact version, which rewrites its outgoing edges.
	//
	// kb/c.md specifically: it was written last, so it already saw every other
	// fact and its rewrite is a true no-op. kb/a.md was written first with no
	// neighbours to find, so re-indexing it would legitimately ADD edges and the
	// final equality below would be measuring that instead of the commit.
	const rewritten = "kb/c.md"
	var blob string
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT blob_hash FROM facts WHERE path = ?`, rewritten).Scan(&blob))
	require.Greater(t, similarToOutDegree(t, svc, ctx, rewritten, blob), 0,
		"the rewritten version must have edges, or the delete under test is a no-op")
	require.NoError(t, svc.si.graphBuildSimilarityEdges(ctx, rewritten, blob))

	require.True(t, fired, "the in-transaction hook must have run, else nothing was observed")
	require.Equal(t, baseline, seenMidRewrite,
		"a second connection must not see the delete before the re-merge commits")
	require.Equal(t, baseline, countSimilarOn(t, obs, ctx),
		"the committed layer must be unchanged by an idempotent rewrite")
}

// Rebuild's similarity phase has the same shape at a larger scale: prune, then
// re-merge, inside one transaction. Same guarantee, same observation point —
// this is what inSimTx exists for.
func TestRebuildGraph_SimilarityPruneIsInvisibleUntilCommit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md"} {
		_, err = svc.Facts().WriteFact(ctx, "main", p,
			testFactBody("shared subject matter for knn", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	obs := openObserver(t, dbPath)
	baseline := countSimilarOn(t, obs, ctx)
	require.Greater(t, baseline, 0,
		"the similarity layer must be populated, or the observation below is vacuous")

	var seenMidRebuild int
	fired := false
	svc.si.inSimTx = func() {
		if fired {
			return
		}
		fired = true
		seenMidRebuild = countSimilarOn(t, obs, ctx)
	}
	defer func() { svc.si.inSimTx = nil }()

	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	require.True(t, fired, "the in-transaction hook must have run, else nothing was observed")
	require.Equal(t, baseline, seenMidRebuild,
		"a second connection must not see the prune before the re-merge commits")
	require.Equal(t, baseline, countSimilarOn(t, obs, ctx),
		"a repeated Rebuild must leave the committed layer identical")
}
