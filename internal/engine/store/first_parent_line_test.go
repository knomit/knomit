package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/model"
)

// The three history readers that used to run vcs.FirstParentChainCTE JOIN
// commit_log ... ORDER BY fpc.depth LIMIT n, and now share vcs.LineEvents
// (commit_fp jump pointers, the commit_parents walk as fallback). Each oracle
// below is the reader's old query, verbatim.

// firstParentChainCTE is the first-parent walk the history readers ran
// before vcs.LineEvents (each with its own SELECT ... ORDER BY fpc.depth): it
// binds the start commit and yields fpc(commit_hash, depth) over
// commit_parents, parent_order = 0. The oracles below are built on it.
const firstParentChainCTE = `
	WITH RECURSIVE fpc(commit_hash, depth) AS (
	    SELECT ?, 0
	    UNION ALL
	    SELECT cp.parent_hash, fpc.depth + 1
	      FROM commit_parents cp
	      JOIN fpc ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
	)`

// oracleActiveCommit is ResolveActiveCommitForPath's old query.
func oracleActiveCommit(t *testing.T, db *sql.DB, path, from string) (string, bool) {
	t.Helper()
	var hash string
	err := db.QueryRow(firstParentChainCTE+`
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

// oracleLiveAt is FactLiveAtCommit's old query.
func oracleLiveAt(t *testing.T, db *sql.DB, path, from string) bool {
	t.Helper()
	var action string
	err := db.QueryRow(firstParentChainCTE+`
		SELECT cl.action
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		 WHERE cl.path = ?
		 ORDER BY fpc.depth ASC
		 LIMIT 1`, from, path).Scan(&action)
	if err == sql.ErrNoRows {
		return false
	}
	require.NoError(t, err)
	return action == "added" || action == "modified"
}

// oracleRevisionsBefore is RevisionsBefore's old query (message first line
// applied as RevisionsBefore does).
func oracleRevisionsBefore(t *testing.T, db *sql.DB, branchID int64, path, from string, limit int) []model.RevisionMeta {
	t.Helper()
	rows, err := db.Query(firstParentChainCTE+`
		SELECT cl.commit_hash, COALESCE(cl.committed_at, 0), cl.message, cl.action
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		  JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		 WHERE bc.branch_id = ? AND cl.path = ? AND cl.action IN ('added','modified')
		 ORDER BY fpc.depth ASC
		 LIMIT ?`, from, branchID, path, limit)
	require.NoError(t, err)
	defer rows.Close()
	var out []model.RevisionMeta
	for rows.Next() {
		var rm model.RevisionMeta
		var msg string
		require.NoError(t, rows.Scan(&rm.Commit, &rm.CommittedAt, &msg, &rm.Action))
		rm.Message = vcs.FirstLine(msg)
		out = append(out, rm)
	}
	require.NoError(t, rows.Err())
	return out
}

func lineCommits(evs []vcs.LineEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Commit)
	}
	return out
}

func revCommits(revs []model.RevisionMeta) []string {
	var out []string
	for _, r := range revs {
		out = append(out, r.Commit)
	}
	return out
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// checkAllReaders holds every migrated reader — its production entry point,
// its commit_fp form alone and its walk alone — to its old query for every
// (path, commit, branch[, limit]) of the store. requireJumps demands the
// commit_fp form answer in full (no fallback) for every commit with an
// undegraded commit_fp row. It returns, per reader, how many lookups had an
// answer, so a caller can require non-vacuous coverage.
func checkAllReaders(t *testing.T, svc *Service, name string, requireJumps bool) (resolved, live, revs int) {
	t.Helper()
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	db := rh.DB()
	commits := queryStrings(t, db, `SELECT DISTINCT commit_hash FROM branch_commits`)
	sort.Strings(commits)
	paths := append([]string{"kb/never.md"}, queryStrings(t, db, `SELECT DISTINCT path FROM commit_log ORDER BY path`)...)
	branches := queryStrings(t, db, `SELECT name FROM branches ORDER BY name`)
	undegraded := map[string]bool{}
	for _, c := range queryStrings(t, db, `SELECT commit_hash FROM commit_fp WHERE degraded <> 2`) {
		undegraded[c] = true
	}
	jumpsMustAnswer := func(c string) bool { return requireJumps && undegraded[c] }

	for _, c := range commits {
		for _, p := range paths {
			// ResolveActiveCommitForPath.
			want, wantOK := oracleActiveCommit(t, db, p, c)
			got, gotOK, err := rh.ResolveActiveCommitForPath(ctx, "main", p, c)
			require.NoError(t, err)
			require.Equal(t, wantOK, gotOK, "%s: resolve %s at %s", name, p, c[:8])
			require.Equal(t, want, got, "%s: resolve %s at %s", name, p, c[:8])
			if gotOK {
				resolved++
			}
			wq := vcs.LineQuery{Path: p, Writes: true, Limit: 1}
			j, complete, _, err := rh.LineEventsJumpsCountingForTest(ctx, c, wq)
			require.NoError(t, err)
			if jumpsMustAnswer(c) {
				require.True(t, complete, "%s: jumps answer in full at %s", name, c[:8])
			}
			if complete {
				require.Equal(t, want, firstOr(lineCommits(j)), "%s: resolve jumps %s at %s", name, p, c[:8])
			}
			w, err := rh.LineEventsWalkForTest(ctx, c, wq)
			require.NoError(t, err)
			require.Equal(t, want, firstOr(lineCommits(w)), "%s: resolve walk %s at %s", name, p, c[:8])

			// FactLiveAtCommit.
			wantLive := oracleLiveAt(t, db, p, c)
			gotLive, err := svc.FactQuery().FactLiveAtCommit(ctx, "main", p, c)
			require.NoError(t, err)
			require.Equal(t, wantLive, gotLive, "%s: live %s at %s", name, p, c[:8])
			if gotLive {
				live++
			}
			aq := vcs.LineQuery{Path: p, Limit: 1}
			want1 := ""
			if evs, err := rh.LineEventsWalkForTest(ctx, c, aq); err == nil && len(evs) > 0 {
				want1 = evs[0].Action
			}
			if wantLive {
				require.Contains(t, []string{"added", "modified"}, want1, "%s: live walk %s at %s", name, p, c[:8])
			} else {
				require.NotContains(t, []string{"added", "modified"}, want1, "%s: live walk %s at %s", name, p, c[:8])
			}
			ja, complete, _, err := rh.LineEventsJumpsCountingForTest(ctx, c, aq)
			require.NoError(t, err)
			if jumpsMustAnswer(c) {
				require.True(t, complete)
			}
			if complete {
				require.Equal(t, want1, firstAction(ja), "%s: live jumps %s at %s", name, p, c[:8])
			}

			// RevisionsBefore, on every branch, at several limits.
			for _, b := range branches {
				bid, err := rh.BranchID(ctx, b)
				require.NoError(t, err)
				for _, limit := range []int{1, 2, 3, 50} {
					want := oracleRevisionsBefore(t, db, bid, p, c, limit)
					got, err := svc.Search().RevisionsBefore(ctx, b, p, c, limit)
					require.NoError(t, err)
					require.Equal(t, want, got, "%s: revisions %s at %s on %s limit %d", name, p, c[:8], b, limit)
					revs += len(got)
					rq := vcs.LineQuery{Path: p, Writes: true, BranchID: bid, Limit: limit}
					jr, complete, _, err := rh.LineEventsJumpsCountingForTest(ctx, c, rq)
					require.NoError(t, err)
					if jumpsMustAnswer(c) {
						require.True(t, complete)
					}
					if complete {
						require.Equal(t, revCommits(want), lineCommits(jr), "%s: revisions jumps %s at %s on %s limit %d", name, p, c[:8], b, limit)
					}
					wr, err := rh.LineEventsWalkForTest(ctx, c, rq)
					require.NoError(t, err)
					require.Equal(t, revCommits(want), lineCommits(wr), "%s: revisions walk %s at %s on %s limit %d", name, p, c[:8], b, limit)
				}
			}
		}
	}
	return resolved, live, revs
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func firstAction(evs []vcs.LineEvent) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[0].Action
}

// Every migrated reader — ResolveActiveCommitForPath, FactLiveAtCommit and
// RevisionsBefore (every branch, limits 1, 2, 3, 50) — answers exactly what its
// old ORDER BY fpc.depth query answers, through its production entry point,
// its commit_fp form alone and its walk alone, for EVERY (path, commit) any
// branch reaches. The history has a modify, a revert to an earlier blob, a
// delete, a rename (delete + add in one commit), a re-add after the delete, a
// real merge that carries a modification of a path both sides have, more
// off-line writes of one path than one candidate page holds, and commits on
// both branches. It is checked as indexed, with every commit forced into ONE
// second, and with commit times reversed (descendants older than their
// ancestors): the readers order by first-parent depth, never committed_at.
//
// Named pairs pin WHICH answer each reader gives, so an oracle and a reader
// sharing one bug still fail.
//
// Sabotage (each fails this test): accept a candidate without the
// first-parent-line check; stop after the first candidate page; drop the
// Writes filter (deletes count as writes); drop the branch filter; order
// candidates by committed_at instead of depth; return before Limit rows.
func TestFirstParentLine_ReadersEqualTheirOldQueries(t *testing.T) {
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
	var a3Depth, offLineAbove, degraded int
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, a3).Scan(&a3Depth))
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM commit_log cl JOIN commit_fp fp ON fp.commit_hash = cl.commit_hash
		 WHERE cl.path = 'kb/s.md' AND cl.action IN ('added','modified') AND fp.depth <= ? AND cl.commit_hash <> ?`,
		a3Depth, sRoot).Scan(&offLineAbove))
	require.Greater(t, offLineAbove, 16, "precondition: kb/s.md at a3 has more off-line candidates than one page")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM commit_fp WHERE degraded <> 0`).Scan(&degraded))
	require.Zero(t, degraded, "precondition: nothing degraded, so the commit_fp form must answer everything")

	mainID, err := rh.BranchID(ctx, "main")
	require.NoError(t, err)
	featureID, err := rh.BranchID(ctx, "feature")
	require.NoError(t, err)
	_ = mainID

	named := func(phase string) {
		resolve := []struct{ path, at, want string }{
			{"kb/a.md", a1, a1}, {"kb/a.md", x1, a1}, {"kb/a.md", a2, a2},
			{"kb/a.md", aRevert, aRevert}, {"kb/a.md", aDel, aRevert}, {"kb/a.md", rename, aRevert},
			{"kb/a.md", a3, a3}, {"kb/a.md", after, a3},
			{"kb/x.md", rename, x1}, {"kb/z.md", rename, rename}, {"kb/z.md", after, rename},
			{"kb/b.md", mergeC, mergeC}, {"kb/b.md", a3, ""}, {"kb/b.md", fS, fB},
			{"kb/s.md", mergeC, mergeC}, {"kb/s.md", after, mergeC},
			{"kb/s.md", a3, sRoot}, // past more than a page of off-line writes
		}
		for _, n := range resolve {
			got, ok, err := rh.ResolveActiveCommitForPath(ctx, "main", n.path, n.at)
			require.NoError(t, err)
			require.Equal(t, n.want != "", ok, "%s: resolve %s at %s", phase, n.path, n.at[:8])
			require.Equal(t, n.want, got, "%s: resolve %s at %s", phase, n.path, n.at[:8])
		}
		liveAt := []struct {
			path, at string
			want     bool
		}{
			{"kb/a.md", aRevert, true}, {"kb/a.md", aDel, false}, {"kb/a.md", rename, false},
			{"kb/a.md", a3, true}, {"kb/x.md", x1, true}, {"kb/x.md", rename, false},
			{"kb/z.md", rename, true}, {"kb/b.md", a3, false}, {"kb/b.md", mergeC, true},
		}
		for _, n := range liveAt {
			got, err := svc.FactQuery().FactLiveAtCommit(ctx, "main", n.path, n.at)
			require.NoError(t, err)
			require.Equal(t, n.want, got, "%s: live %s at %s", phase, n.path, n.at[:8])
		}
		revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/a.md", after, 50)
		require.NoError(t, err)
		require.Equal(t, []string{a3, aRevert, a2, a1}, revCommits(revs), "%s: a's revisions on main", phase)
		revs, err = svc.Search().RevisionsBefore(ctx, "main", "kb/a.md", after, 2)
		require.NoError(t, err)
		require.Equal(t, []string{a3, aRevert}, revCommits(revs), "%s: limit 2", phase)
		// Branch scoping: from main's merge, feature sees only its own rows
		// on that line (the merge is not on feature; the fork-point write is).
		revs, err = svc.Search().RevisionsBefore(ctx, "feature", "kb/s.md", mergeC, 50)
		require.NoError(t, err)
		require.Equal(t, []string{sRoot}, revCommits(revs), "%s: s on feature from main's merge", phase)
		_ = featureID
	}

	for _, phase := range []string{"as indexed", "same second", "reversed times"} {
		switch phase {
		case "same second":
			_, err = db.Exec(`UPDATE commit_log SET committed_at = 1000`)
			require.NoError(t, err)
		case "reversed times":
			_, err = db.Exec(`UPDATE commit_log SET committed_at =
				100000 - 10 * (SELECT depth FROM commit_fp fp WHERE fp.commit_hash = commit_log.commit_hash)`)
			require.NoError(t, err)
		}
		named(phase)
		resolved, live, revs := checkAllReaders(t, svc, phase, true)
		require.Positive(t, resolved, phase)
		require.Positive(t, live, phase)
		require.Positive(t, revs, phase)
	}
}

// A commit with NO commit_fp row, and a STRUCTURALLY DEGRADED one
// (commit_fp.degraded = 2: a parent commit was missing when it was derived,
// so its depth and jump pointers are cut at that boundary), fall back to the
// commit_parents walk wherever the jumps cannot answer in full, and every
// reader still answers exactly its old query.
//
// Fixture: o (writes kb/old.md), c1..c4 (write kb/t.md); c2's commit object
// is removed and the history re-derived, so c3 and c4 are structurally
// degraded (c3's depth restarts at 0) and o and c1, below the boundary, have
// no commit_fp row. commit_parents and commit_log still hold every edge and
// row.
//
// Sabotage (each fails this test): ignore degraded (trust the jumps' partial
// answer) — kb/old.md from c4 answers nothing instead of o; treat a missing
// commit_fp row as "nothing here" instead of falling back — kb/old.md from c1
// answers nothing.
func TestFirstParentLine_DegradedAndUnderivedFallBack(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	rh := svc.si.RepoForTest()
	db := rh.DB()
	o := writeMergeFact(t, svc, "main", "kb/old.md", "old", "old")
	c := []string{o}
	for i := 1; i <= 4; i++ {
		c = append(c, writeMergeFact(t, svc, "main", "kb/t.md", "t", fmt.Sprintf("t %d", i)))
	}
	removeObject(t, svc, plumbing.NewHash(c[2]))
	staleVersion(t, svc)
	require.NoError(t, svc.rh.OpenHistory(ctx))

	// Preconditions: the damage has the intended shape.
	fp := func(h string) (depth, degraded int, ok bool) {
		err := db.QueryRow(`SELECT depth, degraded FROM commit_fp WHERE commit_hash = ?`, h).Scan(&depth, &degraded)
		if err == sql.ErrNoRows {
			return 0, 0, false
		}
		require.NoError(t, err)
		return depth, degraded, true
	}
	for _, h := range []string{c[3], c[4]} {
		_, deg, ok := fp(h)
		require.True(t, ok)
		require.Equal(t, 2, deg, "precondition: %s is structurally degraded", h[:8])
	}
	d3, _, _ := fp(c[3])
	require.Zero(t, d3, "precondition: the depth restarts at the boundary")
	for _, h := range []string{o, c[1]} {
		_, _, ok := fp(h)
		require.False(t, ok, "precondition: %s, below the boundary, is underived", h[:8])
	}
	got, ok := oracleActiveCommit(t, db, "kb/old.md", c[4])
	require.True(t, ok)
	require.Equal(t, o, got, "precondition: commit_parents still reaches o from c4")

	// Degraded: the jumps see only c3, c4 and cannot answer kb/old.md...
	_, complete, _, err := rh.LineEventsJumpsCountingForTest(ctx, c[4], vcs.LineQuery{Path: "kb/old.md", Writes: true, Limit: 1})
	require.NoError(t, err)
	require.False(t, complete, "degraded with no row found above the boundary: fall back")
	// ...but answer kb/t.md in full above the boundary.
	j, complete, _, err := rh.LineEventsJumpsCountingForTest(ctx, c[4], vcs.LineQuery{Path: "kb/t.md", Writes: true, Limit: 2})
	require.NoError(t, err)
	require.True(t, complete, "rows found above the boundary need no fallback")
	require.Equal(t, []string{c[4], c[3]}, lineCommits(j))
	_, complete, _, err = rh.LineEventsJumpsCountingForTest(ctx, c[4], vcs.LineQuery{Path: "kb/t.md", Writes: true, Limit: 3})
	require.NoError(t, err)
	require.False(t, complete, "a third revision lies past the boundary: fall back")
	// Underived: no row, no jumps.
	_, complete, _, err = rh.LineEventsJumpsCountingForTest(ctx, c[1], vcs.LineQuery{Path: "kb/old.md", Writes: true, Limit: 1})
	require.NoError(t, err)
	require.False(t, complete, "no commit_fp row: fall back")

	// The production readers.
	got, ok, err = rh.ResolveActiveCommitForPath(ctx, "main", "kb/old.md", c[4])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, o, got, "degraded: resolved through the walk")
	got, ok, err = rh.ResolveActiveCommitForPath(ctx, "main", "kb/old.md", c[1])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, o, got, "underived: resolved through the walk")
	live, err := svc.FactQuery().FactLiveAtCommit(ctx, "main", "kb/old.md", c[4])
	require.NoError(t, err)
	require.True(t, live)
	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c[4], 50)
	require.NoError(t, err)
	require.Equal(t, []string{c[4], c[3], c[2], c[1]}, revCommits(revs), "every revision, past the boundary too")

	// And every pair, every reader, every form, equals its old query.
	resolved, liveN, revN := checkAllReaders(t, svc, "damaged", false)
	require.Positive(t, resolved)
	require.Positive(t, liveN)
	require.Positive(t, revN)
}

// The cost of each migrated reader's lookup is bounded by the path's own
// events and log2 of the depth, not by the length of history. On a 200-commit
// chain whose writes of kb/p.md are at its bottom and near its top, the
// commit_fp form runs at most 2 + Limit × (bits(depth) + 1) statements (depth
// lookup, one candidate page, the jumps per candidate, one row read per hit),
// for each reader's query shape. The walk yields (steps to the Limit-th hit)
// + 1 rows. Counted in statements and rows, not time.
//
// Finally each production entry point must take the commit_fp form: with
// commit_parents emptied the walk is blind past its start, and all three
// readers still answer the bottom write.
//
// Sabotage (each fails this test): test the line by single first-parent
// steps instead of jumps (~100 statements); drop the walk's `hits < limit`
// guard (it yields the whole chain); make LineEvents always walk (the
// emptied commit_parents leaves the readers with nothing).
func TestFirstParentLine_CostIsBounded(t *testing.T) {
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
	mainID, err := rh.BranchID(ctx, "main")
	require.NoError(t, err)

	var tipDepth, firstDepth int
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, tip).Scan(&tipDepth))
	require.NoError(t, db.QueryRow(`SELECT depth FROM commit_fp WHERE commit_hash = ?`, first).Scan(&firstDepth))
	require.GreaterOrEqual(t, tipDepth-firstDepth, filler, "precondition: the bottom write is far below the tip")

	shapes := map[string]vcs.LineQuery{
		"resolve":           {Path: "kb/p.md", Writes: true, Limit: 1},
		"live":              {Path: "kb/p.md", Limit: 1},
		"revisions limit 1": {Path: "kb/p.md", Writes: true, BranchID: mainID, Limit: 1},
		"revisions limit 2": {Path: "kb/p.md", Writes: true, BranchID: mainID, Limit: 2},
	}
	for name, lq := range shapes {
		bound := 2 + lq.Limit*(bits.Len(uint(tipDepth))+1)
		evs, complete, n, err := rh.LineEventsJumpsCountingForTest(ctx, tip, lq)
		require.NoError(t, err)
		require.True(t, complete, name)
		want := []string{writer, first}[:lq.Limit]
		require.Equal(t, want, lineCommits(evs), name)
		require.LessOrEqual(t, n, bound, "%s: statements from the tip", name)
		lq1 := lq
		lq1.Limit = 1
		evs, complete, n, err = rh.LineEventsJumpsCountingForTest(ctx, mid, lq1)
		require.NoError(t, err)
		require.True(t, complete, name)
		require.Equal(t, []string{first}, lineCommits(evs), name)
		require.LessOrEqual(t, n, 2+bits.Len(uint(tipDepth))+1, "%s: statements for a write ~100 steps down", name)
	}
	_, complete, n, err := rh.LineEventsJumpsCountingForTest(ctx, tip, vcs.LineQuery{Path: "kb/never.md", Limit: 1})
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, 2, n, "a path never written: the depth lookup and one empty page")

	// The walk stops at the Limit-th hit.
	walkRows := func(from string, lq vcs.LineQuery) int {
		var c, writes int
		if lq.Writes {
			writes = 1
		}
		require.NoError(t, db.QueryRow(vcs.LineWalkCTE+` SELECT count(*) FROM walk`, from, lq.Path, lq.Limit, lq.BranchID, writes).Scan(&c))
		return c
	}
	var chain int
	require.NoError(t, db.QueryRow(firstParentChainCTE+` SELECT count(*) FROM fpc`, tip).Scan(&chain))
	require.Equal(t, 4, walkRows(tip, shapes["resolve"]), "3 commits, then the writer")
	require.Equal(t, 1, walkRows(writer, shapes["resolve"]))
	require.Equal(t, tipDepth-firstDepth+1, walkRows(tip, shapes["revisions limit 2"]), "down to the second write")
	require.Equal(t, chain, walkRows(tip, vcs.LineQuery{Path: "kb/never.md", Limit: 1}), "a path never written walks the whole chain")

	// The production entry points take the commit_fp form.
	_, err = db.Exec(`DELETE FROM commit_parents`)
	require.NoError(t, err)
	evs, err := rh.LineEventsWalkForTest(ctx, mid, shapes["resolve"])
	require.NoError(t, err)
	require.Empty(t, evs, "precondition: without commit_parents the walk cannot leave mid")
	got, ok, err := rh.ResolveActiveCommitForPath(ctx, "main", "kb/p.md", mid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first, got, "ResolveActiveCommitForPath answers through commit_fp")
	live, err := svc.FactQuery().FactLiveAtCommit(ctx, "main", "kb/p.md", mid)
	require.NoError(t, err)
	require.True(t, live, "FactLiveAtCommit answers through commit_fp")
	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/p.md", tip, 2)
	require.NoError(t, err)
	require.Equal(t, []string{writer, first}, revCommits(revs), "RevisionsBefore answers through commit_fp")
}
