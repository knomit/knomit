package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/merge"
)

// chainWalkActiveCommit is the resolver ResolveActiveCommitForPath used to
// be: the whole first-parent chain joined with commit_log, ordered by depth,
// LIMIT 1. It is the oracle both current forms must agree with.
func chainWalkActiveCommit(t *testing.T, db *sql.DB, path, from string) (string, bool) {
	t.Helper()
	var hash string
	err := db.QueryRow(vcs.FirstParentChainCTE+`
		SELECT cl.commit_hash
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		 WHERE cl.path = ? AND cl.action IN ('added','modified')
		 ORDER BY fpc.depth ASC
		 LIMIT 1`, from, path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false
	}
	require.NoError(t, err)
	return hash, true
}

// ResolveActiveCommitForPath (the commit_fp jump form) AND its fallback (the
// early-stopping commit_parents walk) answer exactly what the old chain walk
// answers, for EVERY (path, commit) pair any branch reaches, over a history
// with a modify, a revert to an earlier blob, a delete, a rename (delete +
// add in one commit), a re-add after the delete, a real merge that carries a
// modification of a path both sides have, more off-line writes of one path
// than one candidate page holds, and commits on both branches. The pairs are
// checked again with every commit forced into ONE second and with commit
// times reversed (descendants older than their ancestors): the resolver
// orders by first-parent depth, never by committed_at.
//
// Beyond equality, named pairs pin WHICH commit is answered, so an oracle and
// a resolver sharing one bug still fail.
//
// Sabotage (each fails this test): accept a candidate without the
// first-parent-line check, and an off-line feature write of kb/s.md is
// answered on main; stop after the first candidate page, and kb/s.md at a3
// (21 off-line candidates above main's own write) answers nothing; count
// 'deleted' rows as writes, and the delete commit answers itself; drop the
// walk's `walk.wrote = 0` guard and take its last wrote row, and the oldest
// write is answered; order candidates by committed_at, and the reversed-times
// phase answers an ancestor.
func TestResolveActiveCommitForPath_EqualsChainWalk(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	db := rh.DB()

	sRoot := writeMergeFact(t, svc, "main", "kb/s.md", "s", "shared one")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	fB := writeMergeFact(t, svc, "feature", "kb/b.md", "b", "b")
	fS := writeMergeFact(t, svc, "feature", "kb/s.md", "s", "shared from feature")
	const offLine = 20
	for i := 0; i < offLine; i++ {
		writeMergeFact(t, svc, "feature", "kb/s.md", "s", fmt.Sprintf("feature s %d", i))
	}

	a1 := writeMergeFact(t, svc, "main", "kb/a.md", "a", "a one")
	x1 := writeMergeFact(t, svc, "main", "kb/x.md", "x", "x")
	a2 := writeMergeFact(t, svc, "main", "kb/a.md", "a", "a two")
	aRevert := writeMergeFact(t, svc, "main", "kb/a.md", "a", "a one") // a1's blob again
	aDel, err := svc.Facts().DeleteFact(ctx, "main", "kb/a.md", "delete a")
	require.NoError(t, err)
	xContent := "---\ntype: observation\n---\n# x\n\nx\n"
	rename, _, err := svc.Facts().BatchWriteFacts(ctx, "main",
		map[string]string{"kb/z.md": xContent}, []string{"kb/x.md"}, "rename x to z", "test")
	require.NoError(t, err)
	// Deepen main past feature, so feature's writes of kb/s.md sit at depths
	// a main commit's lookup must examine (and reject).
	for i := 0; i < offLine; i++ {
		writeMergeFact(t, svc, "main", fmt.Sprintf("kb/m%02d.md", i), "m", "main filler")
	}
	a3 := writeMergeFact(t, svc, "main", "kb/a.md", "a", "a three")
	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyRemoteWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	mergeC, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	parents, _, err := rh.CommitParentsAndTime(mergeC)
	require.NoError(t, err)
	require.Len(t, parents, 2, "precondition: the merge is a real merge commit")
	after := writeMergeFact(t, svc, "main", "kb/y.md", "y", "after the merge")
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	// Precondition: the history really has each event shape.
	actions := map[string]string{}
	rows, err := db.Query(`SELECT commit_hash, path, action FROM commit_log`)
	require.NoError(t, err)
	for rows.Next() {
		var c, p, a string
		require.NoError(t, rows.Scan(&c, &p, &a))
		actions[c+" "+p] = a
	}
	require.NoError(t, rows.Close())
	require.Equal(t, "deleted", actions[aDel+" kb/a.md"], "precondition: a delete row")
	require.Equal(t, "deleted", actions[rename+" kb/x.md"], "precondition: the rename deletes x")
	require.Equal(t, "added", actions[rename+" kb/z.md"], "precondition: the rename adds z")
	require.Equal(t, "modified", actions[aRevert+" kb/a.md"], "precondition: the revert is a modify")
	require.Equal(t, "modified", actions[mergeC+" kb/s.md"], "precondition: the merge carries feature's s")
	require.Contains(t, []string{"added", "modified"}, actions[mergeC+" kb/b.md"], "precondition: the merge brings b")
	var a3Depth, offLineAbove int
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, a3).Scan(&a3Depth))
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM commit_log cl JOIN commit_fp fp ON fp.commit_hash = cl.commit_hash
		 WHERE cl.path = 'kb/s.md' AND cl.action IN ('added','modified') AND fp.depth <= ? AND cl.commit_hash <> ?`,
		a3Depth, sRoot).Scan(&offLineAbove))
	require.Greater(t, offLineAbove, 16, "precondition: kb/s.md at a3 has more off-line candidates than one page")

	// Every commit any branch reaches, and every path ever logged.
	var commits []string
	rows, err = db.Query(`SELECT DISTINCT commit_hash FROM branch_commits`)
	require.NoError(t, err)
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		commits = append(commits, c)
	}
	require.NoError(t, rows.Close())
	sort.Strings(commits)
	require.GreaterOrEqual(t, len(commits), 2*offLine+12)
	paths := []string{"kb/never.md"}
	rows, err = db.Query(`SELECT DISTINCT path FROM commit_log ORDER BY path`)
	require.NoError(t, err)
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		paths = append(paths, p)
	}
	require.NoError(t, rows.Close())

	named := []struct {
		path, at, want string
	}{
		{"kb/a.md", a1, a1},           // the write itself
		{"kb/a.md", x1, a1},           // one step down
		{"kb/a.md", a2, a2},           // a modify
		{"kb/a.md", aRevert, aRevert}, // a revert is a write
		{"kb/a.md", aDel, aRevert},    // the delete is stepped over
		{"kb/a.md", rename, aRevert},  // ... and so are later commits
		{"kb/a.md", a3, a3},           // the re-add
		{"kb/a.md", after, a3},        // across the merge
		{"kb/x.md", rename, x1},       // the renamed-away path: its last write
		{"kb/z.md", rename, rename},   // the renamed-to path: the rename
		{"kb/z.md", after, rename},
		{"kb/b.md", mergeC, mergeC}, // the merge wrote b on main's first-parent line
		{"kb/b.md", a3, ""},         // b is only on feature before the merge
		{"kb/b.md", fS, fB},         // feature's own line
		{"kb/s.md", mergeC, mergeC}, // the merge carried s
		{"kb/s.md", after, mergeC},
		{"kb/s.md", a3, sRoot}, // past more than a page of off-line writes
	}

	phase := func(name string) {
		self, deeper, none := 0, 0, 0
		for _, c := range commits {
			for _, p := range paths {
				want, wantOK := chainWalkActiveCommit(t, db, p, c)
				got, gotOK, err := rh.ResolveActiveCommitForPath(ctx, "main", p, c)
				require.NoError(t, err)
				require.Equal(t, wantOK, gotOK, "%s: %s at %s", name, p, c[:8])
				require.Equal(t, want, got, "%s: %s at %s", name, p, c[:8])
				j, jOK, handled, _, err := rh.ResolveActiveCommitJumpsCountingForTest(ctx, p, c)
				require.NoError(t, err)
				require.True(t, handled, "%s: every indexed commit has a commit_fp row", name)
				require.Equal(t, want, j, "%s: jumps, %s at %s", name, p, c[:8])
				require.Equal(t, wantOK, jOK)
				w, wOK, err := rh.ResolveActiveCommitWalkForTest(ctx, p, c)
				require.NoError(t, err)
				require.Equal(t, want, w, "%s: walk, %s at %s", name, p, c[:8])
				require.Equal(t, wantOK, wOK)
				switch {
				case !gotOK:
					none++
				case got == c:
					self++
				default:
					deeper++
				}
			}
		}
		require.Positive(t, self, "%s: some pairs answer the start commit", name)
		require.Positive(t, deeper, "%s: some pairs resolve further down the chain", name)
		require.Positive(t, none, "%s: some pairs have no write", name)
		for _, n := range named {
			got, ok, err := rh.ResolveActiveCommitForPath(ctx, "main", n.path, n.at)
			require.NoError(t, err)
			require.Equal(t, n.want != "", ok, "%s: %s at %s resolves", name, n.path, n.at[:8])
			require.Equal(t, n.want, got, "%s: %s at %s", name, n.path, n.at[:8])
		}
	}
	phase("as indexed")

	// Every commit in ONE second.
	_, err = db.Exec(`UPDATE commit_log SET committed_at = 1000`)
	require.NoError(t, err)
	phase("same second")

	// Times reversed along first-parent depth: a descendant is OLDER than
	// its ancestors (clock skew), so any committed_at order prefers the
	// ancestor.
	_, err = db.Exec(`UPDATE commit_log SET committed_at =
		100000 - 10 * (SELECT depth FROM commit_fp fp WHERE fp.commit_hash = commit_log.commit_hash)`)
	require.NoError(t, err)
	phase("reversed times")
}

// The cost of a lookup is bounded by the number of the path's own writes and
// log2 of the depth, not by the length of history: from the tip of a
// 200-commit chain whose only write of kb/p.md is at its bottom, the commit_fp
// form runs (depth lookup) + (one candidate page) + (at most one jump per bit
// of the distance) statements. The fallback walk yields (steps to the writer)
// + 1 rows. Counted in statements and rows, not in time.
//
// Finally the production entry point must be the commit_fp form, not the
// walk: with commit_parents emptied, the walk finds nothing below the tip,
// and ResolveActiveCommitForPath still answers the bottom write.
//
// Sabotage (each fails this test): test the first-parent line by single
// first-parent steps (level 0 only) instead of jumps, and the statement count
// is ~200; drop the walk's `walk.wrote = 0` guard, and it yields the whole
// chain; make ResolveActiveCommitForPath always walk, and the emptied
// commit_parents leaves it with no answer.
func TestResolveActiveCommitForPath_CostIsBounded(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	db := rh.DB()

	first := writeMergeFact(t, svc, "main", "kb/p.md", "p", "p one")
	const filler = 200
	var mid string
	for i := 0; i < filler; i++ {
		c := writeMergeFact(t, svc, "main", fmt.Sprintf("kb/f%03d.md", i), "f", "filler")
		if i == filler/2 {
			mid = c
		}
	}
	writer := writeMergeFact(t, svc, "main", "kb/p.md", "p", "p two")
	var tip string
	for i := 0; i < 3; i++ {
		tip = writeMergeFact(t, svc, "main", fmt.Sprintf("kb/g%d.md", i), "g", "after")
	}
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	var tipDepth, firstDepth int
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, tip).Scan(&tipDepth))
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, first).Scan(&firstDepth))
	require.GreaterOrEqual(t, tipDepth-firstDepth, filler, "precondition: the write is far below the tip")

	// The commit_fp form: the nearest write is one jump set away, the bottom
	// write (seen from mid) another; each lookup is a handful of statements.
	bound := func(from int) int { return 2 + bits.Len(uint(from)) }
	got, ok, handled, n, err := rh.ResolveActiveCommitJumpsCountingForTest(ctx, "kb/p.md", tip)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, ok)
	require.Equal(t, writer, got)
	require.LessOrEqual(t, n, bound(tipDepth), "statements for the nearest write from the tip")
	got, ok, handled, n, err = rh.ResolveActiveCommitJumpsCountingForTest(ctx, "kb/p.md", mid)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, ok)
	require.Equal(t, first, got)
	require.LessOrEqual(t, n, bound(tipDepth), "statements for a write ~100 first-parent steps down")
	_, ok, handled, n, err = rh.ResolveActiveCommitJumpsCountingForTest(ctx, "kb/never.md", tip)
	require.NoError(t, err)
	require.True(t, handled)
	require.False(t, ok)
	require.Equal(t, 2, n, "a path never written: the depth lookup and one empty page")

	// The fallback walk stops at the writer.
	walkRows := func(from, path string) int {
		var c int
		require.NoError(t, db.QueryRow(vcs.ActiveCommitWalkCTE+` SELECT count(*) FROM walk`, from, path).Scan(&c))
		return c
	}
	var chain int
	require.NoError(t, db.QueryRow(vcs.FirstParentChainCTE+` SELECT count(*) FROM fpc`, tip).Scan(&chain))
	require.Equal(t, 4, walkRows(tip, "kb/p.md"), "the walk from the tip: 3 commits, then the writer")
	require.Equal(t, 1, walkRows(writer, "kb/p.md"))
	require.Equal(t, chain, walkRows(tip, "kb/never.md"), "a path never written walks the whole chain")

	// The production entry point is the commit_fp form.
	_, err = db.Exec(`DELETE FROM commit_parents`)
	require.NoError(t, err)
	_, ok, err = rh.ResolveActiveCommitWalkForTest(ctx, "kb/p.md", mid)
	require.NoError(t, err)
	require.False(t, ok, "precondition: without commit_parents the walk cannot leave mid")
	got, ok, err = rh.ResolveActiveCommitForPath(ctx, "main", "kb/p.md", mid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first, got, "ResolveActiveCommitForPath answers through commit_fp")
}
