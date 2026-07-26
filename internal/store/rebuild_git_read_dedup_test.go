package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuildGraph_ReadsEachCommitPathAtMostOnce pins the P2 git-read dedupe.
//
// rebuildGraph runs two phases that BOTH walk the identical commit_log query
// (Phase A.5 restores historical Fact nodes; Phase B writes DERIVED_FROM
// edges). Historically each phase independently called readBlobHashAtCommit +
// readFileAtCommit per (commit, path), so every commit_log ref-event was read
// from git ~3× (Phase A.5: one blob read/event + content for historical
// versions; Phase B: one content + one blob read/event). The fix walks the
// union of events once, caching blob_hash + parsed content, so each unique
// (commit, path) costs at most one blob read + one content read — a bound of
// 2× the commit_log ref-event count.
//
// The gitTreeReads counter on repoHandler (branch.go) is the observability
// seam: it increments once per readFileAtCommit / readBlobHashAtCommit tree
// walk. Within rebuildGraph, the shared walk reads happen ONLY in Phase A.5
// and Phase B, so measuring the counter delta across a single rebuildGraph
// call isolates exactly the double-walk.
//
// The corpus is deliberately ref-free: DERIVED_FROM edge writes call
// resolveTargetCommit -> readBlobHashAtCommit per ref target (derived_from.go),
// which are inherent edge-resolution reads NOT attributable to the walk. This
// test isolates the walk dedup; edge-writing-with-cache correctness is locked
// separately by TestRebuildGraph_RestoresHistoricalFactNodes (which has refs,
// historical versions, and asserts the edges).
func TestRebuildGraph_ReadsEachCommitPathAtMostOnce(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Build a corpus with historical (updated) versions but NO refs, so both
	// phases walk the full commit_log but no edge-target resolution occurs.
	//   A v1, A v2  -> A_v1 becomes historical
	//   B v1, B v2  -> B_v1 becomes historical
	//   C v1        -> current only
	write := func(path, title, msg string) {
		t.Helper()
		_, werr := svc.Facts().WriteFact(ctx, branch, path,
			testFactBody(title, 0.9, nil), msg, "")
		require.NoError(t, werr)
	}
	write("kb/a.md", "a v1", "init a")
	write("kb/b.md", "b v1", "init b")
	write("kb/a.md", "a v2", "update a")
	write("kb/b.md", "b v2", "update b")
	write("kb/c.md", "c v1", "init c")

	si := svc.si

	// N = number of (commit, path) ref-events rebuildGraph's two phases each
	// walk. This is the SAME query both phases run; computing it here keeps
	// the bound honest instead of hard-coding a magic number.
	var n int
	require.NoError(t, si.rh.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = (SELECT id FROM branches WHERE name = ?)
		  AND cl.action != 'deleted'`, branch).Scan(&n))
	require.Greater(t, n, 0, "corpus must produce commit_log events")

	before := si.rh.gitTreeReadCount()
	_, err = si.rebuildGraph(ctx, branch, nil)
	require.NoError(t, err)
	delta := si.rh.gitTreeReadCount() - before

	// A single-walk rebuildGraph reads each unique (commit, path) at most
	// once for its blob hash and once for its content: 2×N. The old
	// double-walk reads ~3×N, so this bound fails until the phases share one
	// walk + a parsed-fact cache.
	require.LessOrEqualf(t, delta, int64(2*n),
		"rebuildGraph made %d git tree reads for %d commit_log events (bound %d = 2×N); "+
			"Phase A.5 and Phase B are re-reading the same (commit, path) from git",
		delta, n, 2*n)
}
