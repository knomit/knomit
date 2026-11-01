package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/internal/vcs"
)

// fpRow reads a commit's commit_fp row; ok is false when it has none.
func fpRow(t *testing.T, svc *Service, h string) (depth, degraded int, ups []byte, ok bool) {
	t.Helper()
	err := svc.rh.DB().QueryRow(`SELECT depth, degraded, ups FROM commit_fp WHERE commit_hash = ?`, h).Scan(&depth, &degraded, &ups)
	if err != nil {
		return 0, 0, nil, false
	}
	return depth, degraded, ups, true
}

// requireReadersAgreeAt holds every migrated reader, and LiveRevision, to
// the old queries for path at from on main.
func requireReadersAgreeAt(t *testing.T, svc *Service, path, from string) {
	t.Helper()
	requireMigratedReadersAgreeAt(t, svc, path, from)
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	mainID, err := rh.BranchID(ctx, "main")
	require.NoError(t, err)
	wantRevs := oracleRevisionsBefore(t, rh.DB(), mainID, path, from, 1)
	lr, err := svc.Search().LiveRevision(ctx, "main", path, from)
	require.NoError(t, err)
	require.Equal(t, firstOr(revCommits(wantRevs)), lr, "LiveRevision %s at %s", path, from[:8])
}

// requireMigratedReadersAgreeAt holds the three LineEvents readers to the
// old queries for path at from on main. (LiveRevision reads path_changes
// through LiveChange, which has no walk to fall back on: hand-made
// inconsistent rows are out of its contract; the re-derivation fix keeps a
// real store from producing them.)
func requireMigratedReadersAgreeAt(t *testing.T, svc *Service, path, from string) {
	t.Helper()
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	db := rh.DB()
	want, wantOK := oracleActiveCommit(t, db, path, from)
	got, ok, err := rh.ResolveActiveCommitForPath(ctx, "main", path, from)
	require.NoError(t, err)
	require.Equal(t, wantOK, ok, "resolve %s at %s", path, from[:8])
	require.Equal(t, want, got, "resolve %s at %s", path, from[:8])
	mainID, err := rh.BranchID(ctx, "main")
	require.NoError(t, err)
	wantRevs := oracleRevisionsBefore(t, db, mainID, path, from, 5)
	revs, err := svc.Search().RevisionsBefore(ctx, "main", path, from, 5)
	require.NoError(t, err)
	require.Equal(t, revCommits(wantRevs), revCommits(revs), "revisions %s at %s", path, from[:8])
	live, err := svc.FactQuery().FactLiveAtCommit(ctx, "main", path, from)
	require.NoError(t, err)
	require.Equal(t, oracleLiveAt(t, db, path, from), live, "live %s at %s", path, from[:8])
}

// b1Store: o writes kb/p.md, P is filler (A's first parent), A writes
// kb/p.md, then five commits above A.
func b1Store(t *testing.T) (svc *Service, o, p, a, tip string) {
	t.Helper()
	svc, _ = openPathHistoryStore(t)
	o = writeMergeFact(t, svc, "main", "kb/p.md", "p", "p below")
	p = writeMergeFact(t, svc, "main", "kb/f.md", "f", "filler (A's parent)")
	a = writeMergeFact(t, svc, "main", "kb/p.md", "p", "p at A")
	for i := 0; i < 5; i++ {
		tip = writeMergeFact(t, svc, "main", fmt.Sprintf("kb/d%d.md", i), "d", "above")
	}
	return svc, o, p, a, tip
}

// B1 (review of #698), regression. A commit A with degraded = 1 whose first
// parent's commit object then goes missing must NOT be re-derived into a
// boundary (degraded = 2, depth 0, no pointers): its descendants were derived
// over its depth and pointers and keep degraded = 0, so the readers would
// trust them and skip A's write (they answered o instead of A, LiveRevision
// included). Re-derivation keeps A's row as it was
// (deriver.wouldCutDerivedLine), and every reader, LiveRevision included,
// answers what the old queries answer.
//
// Two routes: :rebuild (RebuildCommitLog) on real damage — a missing blob at
// derivation, then a missing parent commit — and the re-derivation pass
// alone on a hand-marked degraded = 1 row. RebuildCommitLog now walks git
// before it writes anything, so its failure changes no row.
//
// Sabotage (each fails this test): drop wouldCutDerivedLine (re-derive
// anyway), and A becomes depth 0 / degraded 2 under undegraded descendants
// and the readers answer o; run rederiveDegraded before the walk in
// RebuildCommitLog again, and the rebuild that fails at the walk has already
// repaired C (the "changes no row" subtest).
func TestFirstParentLine_RederiveNeverCutsALineUnderItsDescendants(t *testing.T) {
	t.Run("rebuild on real damage", func(t *testing.T) {
		svc, o, p, a, tip := b1Store(t)
		ctx := context.Background()
		removeObject(t, svc, plumbing.NewHash(blobOf(t, svc, a, "kb/p.md")))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		aDepth, aDeg, aUps, ok := fpRow(t, svc, a)
		require.True(t, ok)
		require.Equal(t, 1, aDeg, "precondition: A content-degraded")
		_, tipDeg, _, _ := fpRow(t, svc, tip)
		require.Equal(t, 0, tipDeg, "precondition: tip undegraded")

		removeObject(t, svc, plumbing.NewHash(p))
		require.Error(t, svc.rh.RebuildCommitLog(ctx, "main"), "git cannot walk past the missing commit")
		d, deg, ups, ok := fpRow(t, svc, a)
		require.True(t, ok)
		require.Equal(t, aDepth, d, "A keeps its depth")
		require.Equal(t, aDeg, deg, "A keeps its mark")
		require.Equal(t, aUps, ups, "A keeps its jump pointers")
		for _, path := range []string{"kb/p.md", "kb/f.md", "kb/d2.md"} {
			for _, from := range []string{tip, a, o} {
				requireReadersAgreeAt(t, svc, path, from)
			}
		}
		got, _, err := svc.si.RepoForTest().ResolveActiveCommitForPath(ctx, "main", "kb/p.md", tip)
		require.NoError(t, err)
		require.Equal(t, a, got, "A, never the superseded o")
	})
	t.Run("rederive pass on a degraded=1 row", func(t *testing.T) {
		svc, o, p, a, tip := b1Store(t)
		ctx := context.Background()
		rh := svc.si.RepoForTest()
		_, err := rh.DB().Exec(`UPDATE commit_fp SET degraded = 1 WHERE commit_hash = ?`, a)
		require.NoError(t, err)
		aDepth, _, aUps, _ := fpRow(t, svc, a)
		removeObject(t, svc, plumbing.NewHash(p))
		_, err = rh.RederiveDegradedForTest(ctx)
		require.NoError(t, err)
		d, deg, ups, ok := fpRow(t, svc, a)
		require.True(t, ok)
		require.Equal(t, aDepth, d, "A keeps its depth")
		// The mark was set by hand over complete content: the kept row's
		// content refresh (NB1) finds nothing missing and clears it, while
		// depth and pointers stay.
		require.Equal(t, 0, deg, "no content is missing: the kept row's mark clears")
		require.Equal(t, aUps, ups)
		for _, from := range []string{tip, a, o} {
			requireReadersAgreeAt(t, svc, "kb/p.md", from)
		}
		lr, err := svc.Search().LiveRevision(ctx, "main", "kb/p.md", tip)
		require.NoError(t, err)
		require.Equal(t, a, lr, "LiveRevision answers A too")
	})
	t.Run("a rebuild that git cannot walk changes no row", func(t *testing.T) {
		// C's blob is missing at derivation (degraded = 1) and comes back; an
		// older commit P goes missing, so git cannot walk the branch. The
		// rebuild fails BEFORE re-deriving: C is not repaired by a rebuild
		// that reports failure.
		svc, ctx := openPathHistoryStore(t)
		writeMergeFact(t, svc, "main", "kb/o.md", "o", "o")
		p := writeMergeFact(t, svc, "main", "kb/f.md", "f", "P")
		writeMergeFact(t, svc, "main", "kb/x.md", "x", "x")
		c := writeMergeFact(t, svc, "main", "kb/c.md", "c", "C")
		writeMergeFact(t, svc, "main", "kb/y.md", "y", "above")
		restoreBlob := removeObject(t, svc, plumbing.NewHash(blobOf(t, svc, c, "kb/c.md")))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		_, deg, _, _ := fpRow(t, svc, c)
		require.Equal(t, 1, deg, "precondition: C content-degraded")
		restoreBlob()
		removeObject(t, svc, plumbing.NewHash(p))
		require.Error(t, svc.rh.RebuildCommitLog(ctx, "main"))
		_, deg, _, _ = fpRow(t, svc, c)
		require.Equal(t, 1, deg, "the failed rebuild re-derived nothing")
	})
	t.Run("a structural row is still repaired once its parent is back", func(t *testing.T) {
		svc, o, p, a, tip := b1Store(t)
		ctx := context.Background()
		restore := removeObject(t, svc, plumbing.NewHash(p))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		_, deg, _, _ := fpRow(t, svc, a)
		require.Equal(t, 2, deg, "precondition: A is a boundary")
		restore()
		require.NoError(t, svc.rh.RebuildCommitLog(ctx, "main"))
		for _, h := range []string{a, tip} {
			_, deg, _, ok := fpRow(t, svc, h)
			require.True(t, ok)
			require.Equal(t, 0, deg, "%s repaired", h[:8])
		}
		dA, _, _, _ := fpRow(t, svc, a)
		dT, _, _, _ := fpRow(t, svc, tip)
		require.Equal(t, dA+5, dT, "the tip's depth agrees with A's again")
		requireReadersAgreeAt(t, svc, "kb/p.md", tip)
		requireReadersAgreeAt(t, svc, "kb/p.md", o)
	})
}

// Guard (c): a jump that the rows contradict — a node on the way whose depth
// is not its distance from the start, or that has no pointer for the next
// jump — makes the commit_fp form report incomplete, and the readers walk
// commit_parents instead. Fixture o (writes kb/p.md), P, A, tip: from tip,
// candidate o is 3 steps down, so the jump takes tip's level-0 pointer to A
// and then A's level-1 pointer: A's row is on the way.
//
// Sabotage (each fails this test): drop the depth check, and the depth
// contradiction is answered from the jumps; drop the pointer check, and the
// emptied pointers are read past their end.
func TestFirstParentLine_ContradictingJumpWalksInstead(t *testing.T) {
	setup := func(t *testing.T) (*Service, string, string, string) {
		svc, _ := openPathHistoryStore(t)
		o := writeMergeFact(t, svc, "main", "kb/p.md", "p", "p")
		writeMergeFact(t, svc, "main", "kb/f.md", "f", "P")
		a := writeMergeFact(t, svc, "main", "kb/g.md", "g", "A")
		tip := writeMergeFact(t, svc, "main", "kb/h.md", "h", "tip")
		return svc, o, a, tip
	}
	lq := vcs.LineQuery{Path: "kb/p.md", Writes: true, Limit: 1}
	t.Run("depth contradiction", func(t *testing.T) {
		svc, o, a, tip := setup(t)
		rh := svc.si.RepoForTest()
		evs, complete, _, err := rh.LineEventsJumpsCountingForTest(context.Background(), tip, lq)
		require.NoError(t, err)
		require.True(t, complete, "precondition: a consistent store answers from the jumps")
		require.Equal(t, []string{o}, lineCommits(evs))
		_, err = rh.DB().Exec(`UPDATE commit_fp SET depth = depth + 7 WHERE commit_hash = ?`, a)
		require.NoError(t, err)
		_, complete, _, err = rh.LineEventsJumpsCountingForTest(context.Background(), tip, lq)
		require.NoError(t, err)
		require.False(t, complete, "A's depth contradicts its distance from tip")
		requireMigratedReadersAgreeAt(t, svc, "kb/p.md", tip)
	})
	t.Run("pointers run out", func(t *testing.T) {
		svc, _, a, tip := setup(t)
		rh := svc.si.RepoForTest()
		_, err := rh.DB().Exec(`UPDATE commit_fp SET ups = substr(ups, 1, 20) WHERE commit_hash = ?`, a)
		require.NoError(t, err)
		_, complete, _, err := rh.LineEventsJumpsCountingForTest(context.Background(), tip, lq)
		require.NoError(t, err)
		require.False(t, complete, "A has no level-1 pointer")
		requireMigratedReadersAgreeAt(t, svc, "kb/p.md", tip)
	})
	t.Run("the B1 shape itself", func(t *testing.T) {
		svc, _, a, tip := setup(t)
		rh := svc.si.RepoForTest()
		_, err := rh.DB().Exec(`UPDATE commit_fp SET depth = 0, ups = x'', degraded = 2 WHERE commit_hash = ?`, a)
		require.NoError(t, err)
		_, complete, _, err := rh.LineEventsJumpsCountingForTest(context.Background(), tip, lq)
		require.NoError(t, err)
		require.False(t, complete)
		requireMigratedReadersAgreeAt(t, svc, "kb/p.md", tip)
	})
}

// N1: history shapes the equivalence test did not have, each checked with
// every reader against its old query for every (path, commit, branch,
// limit): an octopus merge (3 parents); a tip whose FIRST parent is the
// other line (main moved to a merge made on feature); content-degraded
// commits (degraded = 1, a missing blob) whose jumps must still answer in
// full; and a commit made degraded = 2 by a missing NON-first parent, whose
// first-parent line is intact.
func TestFirstParentLine_MoreHistoryShapes(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	body := func(s string) string { return testFactBody(s, 0.5, nil) }
	t.Run("octopus and first parent on the other line", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		root, err := svc.HeadCommit(ctx, "main")
		require.NoError(t, err)
		base := map[string]string{"kb/a.md": body("a0"), "kb/s.md": body("s0")}
		m1 := rawCommit(t, svc, "main", []string{root}, base, t0, "m1")
		s1 := rawCommit(t, svc, "side1", []string{m1}, map[string]string{"kb/a.md": body("a0"), "kb/s.md": body("s side1"), "kb/x.md": body("x")}, t0.Add(1*time.Minute), "s1")
		s2 := rawCommit(t, svc, "side2", []string{m1}, map[string]string{"kb/a.md": body("a side2"), "kb/s.md": body("s0"), "kb/y.md": body("y")}, t0.Add(2*time.Minute), "s2")
		m2 := rawCommit(t, svc, "main", []string{m1}, map[string]string{"kb/a.md": body("a main"), "kb/s.md": body("s0")}, t0.Add(3*time.Minute), "m2")
		oct := rawCommit(t, svc, "main", []string{m2, s1, s2}, map[string]string{
			"kb/a.md": body("a main"), "kb/s.md": body("s side1"), "kb/x.md": body("x"), "kb/y.md": body("y"),
		}, t0.Add(4*time.Minute), "octopus")
		parents, _, err := svc.si.RepoForTest().CommitParentsAndTime(oct)
		require.NoError(t, err)
		require.Len(t, parents, 3, "precondition: an octopus merge")
		// feature: its own line from m1, then a merge OF main INTO feature;
		// main is then moved to that merge, so main's tip's first parent is
		// feature's line.
		f1 := rawCommit(t, svc, "feature", []string{m1}, map[string]string{"kb/a.md": body("a feature"), "kb/s.md": body("s0"), "kb/f.md": body("f")}, t0.Add(5*time.Minute), "f1")
		fm := rawCommit(t, svc, "feature", []string{f1, oct}, map[string]string{
			"kb/a.md": body("a feature"), "kb/s.md": body("s side1"), "kb/x.md": body("x"), "kb/y.md": body("y"), "kb/f.md": body("f"),
		}, t0.Add(6*time.Minute), "merge main into feature")
		moveBranch(t, svc, "main", fm)
		require.NoError(t, svc.rh.PopulateCommitLog(ctx, "main"))
		tip, err := svc.HeadCommit(ctx, "main")
		require.NoError(t, err)
		require.Equal(t, fm, tip)
		parents, _, err = svc.si.RepoForTest().CommitParentsAndTime(tip)
		require.NoError(t, err)
		require.Equal(t, f1, parents[0], "precondition: main's tip's first parent is feature's line")

		got, ok, err := svc.si.RepoForTest().ResolveActiveCommitForPath(ctx, "main", "kb/a.md", tip)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, f1, got, "kb/a.md at main's tip: feature's write, along the first parent")
		got, _, err = svc.si.RepoForTest().ResolveActiveCommitForPath(ctx, "main", "kb/s.md", oct)
		require.NoError(t, err)
		require.Equal(t, oct, got, "the octopus carried side1's kb/s.md")
		resolved, live, revs := checkAllReaders(t, svc, "octopus+ff", true)
		require.Positive(t, resolved)
		require.Positive(t, live)
		require.Positive(t, revs)
	})
	t.Run("content-degraded commits", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		var cs []string
		for i := 0; i < 6; i++ {
			cs = append(cs, writeMergeFact(t, svc, "main", "kb/t.md", "t", fmt.Sprintf("t %d", i)))
			writeMergeFact(t, svc, "main", fmt.Sprintf("kb/u%d.md", i), "u", "u")
		}
		removeObject(t, svc, plumbing.NewHash(blobOf(t, svc, cs[2], "kb/t.md")))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		var n1, n2 int
		require.NoError(t, svc.rh.DB().QueryRow(`SELECT count(*) FROM commit_fp WHERE degraded = 1`).Scan(&n1))
		require.NoError(t, svc.rh.DB().QueryRow(`SELECT count(*) FROM commit_fp WHERE degraded = 2`).Scan(&n2))
		require.Positive(t, n1, "precondition: content-degraded rows")
		require.Zero(t, n2, "precondition: no boundary")
		// requireJumps: degraded = 1 rows must be answered by the jumps.
		resolved, live, revs := checkAllReaders(t, svc, "degraded=1", true)
		require.Positive(t, resolved)
		require.Positive(t, live)
		require.Positive(t, revs)
	})
	t.Run("missing non-first parent", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		w := writeMergeFact(t, svc, "main", "kb/t.md", "t", "t main")
		require.NoError(t, svc.CreateBranch(ctx, "side", "main"))
		side := writeMergeFact(t, svc, "side", "kb/side.md", "side", "side")
		mainTip, err := svc.HeadCommit(ctx, "main")
		require.NoError(t, err)
		files := map[string]string{}
		for _, p := range []string{"kb/t.md", "kb/side.md"} {
			c, err := svc.rh.ReadFileAtCommit(ctx, p, side)
			require.NoError(t, err)
			files[p] = c
		}
		m := rawCommit(t, svc, "main", []string{mainTip, side}, files, t0, "merge side")
		var above string
		for i := 0; i < 3; i++ {
			above = writeMergeFact(t, svc, "main", fmt.Sprintf("kb/v%d.md", i), "v", "above")
		}
		removeObject(t, svc, plumbing.NewHash(side))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		dm, deg, _, ok := fpRow(t, svc, m)
		require.True(t, ok)
		require.Equal(t, 2, deg, "precondition: the merge is structural (its second parent is missing)")
		dw, _, _, _ := fpRow(t, svc, w)
		require.Greater(t, dm, dw, "precondition: its first-parent depth is intact")
		evs, complete, _, err := svc.si.RepoForTest().LineEventsJumpsCountingForTest(ctx, above, vcs.LineQuery{Path: "kb/t.md", Writes: true, Limit: 1})
		require.NoError(t, err)
		require.True(t, complete, "a write found on the intact first-parent line needs no walk")
		require.Equal(t, []string{w}, lineCommits(evs))
		resolved, live, revs := checkAllReaders(t, svc, "missing second parent", false)
		require.Positive(t, resolved)
		require.Positive(t, live)
		require.Positive(t, revs)
	})
}

// N3: every statement of one LineEvents call reads ONE snapshot. A commit
// that adds a nearer write of the path lands between the call's depth read
// and its candidate page; the call must not see it (the old single statement
// could not), and the next call does.
//
// Sabotage: run LineEvents' statements on the pool instead of one
// read transaction, and the call answers the write committed mid-call.
func TestFirstParentLine_OneCallReadsOneSnapshot(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	ctx := context.Background()
	rh := svc.si.RepoForTest()
	w := writeMergeFact(t, svc, "main", "kb/p.md", "p", "p")
	tip := writeMergeFact(t, svc, "main", "kb/q.md", "q", "q")
	var once sync.Once
	restore := vcs.SetLineEventsAfterDepthHookForTest(func() {
		once.Do(func() {
			_, err := rh.DB().Exec(`INSERT INTO commit_log (commit_hash, path, committed_at, message, action) VALUES (?, 'kb/p.md', 0, 'mid-call', 'modified')`, tip)
			require.NoError(t, err)
		})
	})
	defer restore()
	got, ok, err := rh.ResolveActiveCommitForPath(ctx, "main", "kb/p.md", tip)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, w, got, "the call reads the snapshot it started in")
	got, _, err = rh.ResolveActiveCommitForPath(ctx, "main", "kb/p.md", tip)
	require.NoError(t, err)
	require.Equal(t, tip, got, "precondition: the row was committed; the next call sees it")
}
