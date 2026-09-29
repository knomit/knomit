package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storegit "knomit/internal/store/git"
)

// Tests for derive-at-index (review round 3): a commit's path_changes rows
// are written in the transaction that indexes it, parents first.

// TestPathHistory_TipFirstIndexingNeverTruncates is the regression for review
// N1: a commit derived before its parent was indexed was marked done with an
// unresolved link, and its history stayed [C] instead of [C, P, base] for
// good. Now (1) appending a tip whose ancestors were never indexed indexes the
// ancestors first, and (2) the derive hook refuses a commit whose parent is
// not derived instead of recording a dangling link.
func TestPathHistory_TipFirstIndexingNeverTruncates(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := writeRawCommit(t, svc, nil, map[string]string{p: testFactBody("base", 0.5, nil)}, t0, "base")
	parent := writeRawCommit(t, svc, []string{base}, map[string]string{p: testFactBody("P", 0.6, nil)}, t0.Add(time.Hour), "P")
	tip := writeRawCommit(t, svc, []string{parent}, map[string]string{p: testFactBody("C", 0.7, nil)}, t0.Add(2*time.Hour), "C")
	moveBranch(t, svc, "main", tip)

	// (2) The hook, offered the tip first (the round-2 shape), refuses.
	d := newDeriver(svc.rh)
	sent := false
	err := svc.rh.gits.CommitLogSyncWith(ctx, "main", func() (string, storegit.CommitLogPayload, error) {
		if sent {
			return "", nil, nil
		}
		sent = true
		return tip, func() ([]string, []storegit.CommitLogEntry, error) { return []string{parent}, nil, nil }, nil
	}, storegit.CommitLogSyncOptions{Derive: d.derive})
	require.ErrorIs(t, err, errParentUnderived)
	var marked int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, tip).Scan(&marked))
	require.Zero(t, marked, "the refused commit is neither indexed nor marked: its transaction rolled back")

	// (1) The append path indexes the unindexed ancestors first.
	require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", tip))
	require.Equal(t, []string{tip, parent, base}, historyCommits(fullHistory(t, svc, "main", p, tip)))
}

// TestPathHistory_RebuildIsAtomicAndRepairs covers review N2 and the :rebuild
// requirement: while rebuildCommitLog runs, a concurrent reader sees the full
// history (never a short or empty first page), and afterwards corrupted
// derived rows are repaired.
func TestPathHistory_RebuildIsAtomicAndRepairs(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))
	for k := range 3 {
		for j := range 2 {
			_, err := svc.Facts().WriteFact(ctx, "agent", "kb/t.md", testFactBody(fmt.Sprintf("%d-%d", k, j), 0.5, nil), "w", "")
			require.NoError(t, err)
		}
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/o%d.md", k), testFactBody("o", 0.5, nil), "o", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
	}
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	want := fullHistory(t, svc, "main", "kb/t.md", tip)
	require.Len(t, want, 6)

	// Corrupt the derived rows: a wrong diff, a dropped link.
	_, err = svc.rh.db.Exec(`UPDATE path_changes SET diff = '{"body":"corrupt"}' WHERE path = 'kb/t.md'`)
	require.NoError(t, err)
	_, err = svc.rh.db.Exec(`DELETE FROM path_change_links WHERE commit_hash = ?`, want[0].Commit)
	require.NoError(t, err)

	txCtx, tx, own, err := beginTxIfNeeded(ctx, svc.rh.db)
	require.NoError(t, err)
	require.True(t, own)
	require.NoError(t, svc.rh.rebuildCommitLog(txCtx, "main"))
	// Mid-rebuild, from another connection: the old rows, whole.
	page, _, err := svc.Search().PathHistory(context.Background(), "main", "kb/t.md", tip, nil, 3)
	require.NoError(t, err)
	require.Len(t, page, 1, "mid-rebuild the reader still sees the (corrupted) pre-rebuild rows, not an empty branch")
	require.NoError(t, tx.Commit())

	got := fullHistory(t, svc, "main", "kb/t.md", tip)
	require.Equal(t, historyCommits(want), historyCommits(got), "rebuild re-derived the dropped link")
	for i := range want {
		require.Equal(t, want[i].Diff, got[i].Diff, "rebuild re-derived the diff of %s", got[i].Commit)
	}
}

// TestPathHistory_RewindIsAtomic covers review N2 for a rewind: the purge and
// repopulate run in one transaction, so a reader in between sees the whole
// pre-rewind history, never an empty first page.
func TestPathHistory_RewindIsAtomic(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var commits []string
	for i := range 5 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	tip := commits[4]
	// The ref moves first (as advanceBranchTo does), then the index follows
	// in one transaction.
	moveBranch(t, svc, "main", commits[2])
	txCtx, tx, _, err := beginTxIfNeeded(ctx, svc.rh.db)
	require.NoError(t, err)
	require.NoError(t, svc.rh.repopulateBranch(txCtx, "main"))
	page, _, err := svc.Search().PathHistory(context.Background(), "main", "kb/t.md", tip, nil, 10)
	require.NoError(t, err)
	require.Len(t, page, 5, "mid-rewind the reader sees the complete pre-rewind history")
	require.NoError(t, tx.Commit())

	require.Equal(t, reversed(commits[:3]), historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[2])))
}

// TestPathHistory_UnbackfilledDatabaseDerivesFullHistory is the regression
// for review N4: a database indexed before commit_parents existed — and
// before path_changes existed — derives the whole history when it opens.
func TestPathHistory_UnbackfilledDatabaseDerivesFullHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	var commits []string
	for i := range 4 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	for _, stmt := range []string{
		`DELETE FROM commit_parents`, `DELETE FROM meta WHERE key = 'commit_parents_backfilled'`,
		`DELETE FROM path_changes`, `DELETE FROM path_change_links`, `DELETE FROM commit_fp`, `DELETE FROM commit_fp_up`,
	} {
		_, err := svc.rh.db.Exec(stmt)
		require.NoError(t, err)
	}
	require.NoError(t, svc.Close())

	svc, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	require.Equal(t, reversed(commits), historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[3])))
	fp, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", commits[3], 10)
	require.NoError(t, err)
	require.Len(t, fp, 4, "commit_parents was backfilled")
}

// TestPathHistory_AnchorLeftBranchIsHistoryChanged makes the anchor check
// load-bearing: after a rewind that drops only the anchor, the frontier's
// changes are all still on the branch, so only "the anchor left the branch"
// can refuse the continuation.
func TestPathHistory_AnchorLeftBranchIsHistoryChanged(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var commits []string
	for i := range 6 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	anchor := commits[5]
	_, cur, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", anchor, nil, 2)
	require.NoError(t, err)
	require.Equal(t, []string{commits[3]}, cur.Frontier)

	moveBranch(t, svc, "main", commits[4])
	require.NoError(t, svc.rh.repopulateBranch(ctx, "main"))
	_, _, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", anchor, cur, 2)
	require.ErrorIs(t, err, ErrHistoryChanged, "the frontier is still on the branch; only the anchor check sees the rewind")
}

// TestPathHistory_FrontierOffBranchIsHistoryChanged makes the frontier check
// load-bearing: the anchor is still on the branch, but the position names a
// change visible only on another branch.
func TestPathHistory_FrontierOffBranchIsHistoryChanged(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.5, nil), "v1", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	side, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("side", 0.5, nil), "side", "")
	require.NoError(t, err)

	_, _, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", r1.CommitHash,
		&PathHistoryCursor{Frontier: []string{side.CommitHash}, AnchorOnBranch: true}, 2)
	require.ErrorIs(t, err, ErrHistoryChanged, "the anchor is on the branch; only the frontier check sees the off-branch change")
}

// BenchmarkPathHistoryDeep is the at-scale measurement (review N5/N6): a
// 10,000-commit linear history where every commit edits a noise fact and the
// measured fact changes rarely — once, far back, then far away again, then
// back to its FIRST content (a revert: two entries share a blob), then once
// near the tip. Reports the one-time indexing of all commits, and a history
// first page and cursor page at the tip and at an old non-change anchor.
func BenchmarkPathHistoryDeep(b *testing.B) {
	const n = 10000
	const p = "kb/target.md"
	svc, err := Open(filepath.Join(b.TempDir(), "k.db"))
	require.NoError(b, err)
	defer svc.Close()
	require.NoError(b, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	content := testFactBody("target v1", 0.5, nil)
	var prev []string
	var commits []string
	for i := range n {
		switch i {
		case 3000:
			content = testFactBody("target v2", 0.6, nil)
		case 6000:
			content = testFactBody("target v1", 0.5, nil) // revert to the first content
		case n - 10:
			content = testFactBody("target v3", 0.7, nil)
		}
		h := writeRawCommit(b, svc, prev, map[string]string{
			p:             content,
			"kb/noise.md": testFactBody(fmt.Sprintf("noise %d", i), 0.5, nil),
		}, t0.Add(time.Duration(i)*time.Minute), fmt.Sprintf("c%d", i))
		prev = []string{h}
		commits = append(commits, h)
	}
	moveBranch(b, svc, "main", commits[n-1])
	start := time.Now()
	require.NoError(b, svc.rh.populateCommitLog(ctx, "main"))
	b.Logf("indexed %d commits (commit_log + path_changes) in %v", n, time.Since(start))

	tip := commits[n-1]
	revs := fullHistoryB(b, svc, "main", p, tip)
	require.Len(b, revs, 4, "v3, the revert to v1, v2, v1")
	require.Equal(b, []string{commits[n-10], commits[6000], commits[3000], commits[0]}, historyCommitsB(revs))

	b.Run("FirstPageAtTip", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := svc.Search().PathHistory(ctx, "main", p, tip, nil, 3); err != nil {
				b.Fatal(err)
			}
		}
	})
	_, cur, err := svc.Search().PathHistory(ctx, "main", p, tip, nil, 1)
	require.NoError(b, err)
	b.Run("CursorPage", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := svc.Search().PathHistory(ctx, "main", p, tip, cur, 20); err != nil {
				b.Fatal(err)
			}
		}
	})
	old := commits[5000] // 2000 commits past the last change before it, 1000 before the revert
	b.Run("FirstPageAtOldAnchor", func(b *testing.B) {
		for b.Loop() {
			revs, _, err := svc.Search().PathHistory(ctx, "main", p, old, nil, 3)
			if err != nil || len(revs) != 2 || revs[0].Commit != commits[3000] {
				b.Fatalf("history at an old anchor: %v %v", historyCommitsB(revs), err)
			}
		}
	})
	b.Run("RevisionsBeforeAtTip", func(b *testing.B) {
		for b.Loop() {
			if _, err := svc.Search().RevisionsBefore(ctx, "main", p, tip, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("LiveRevisionAtTip", func(b *testing.B) {
		for b.Loop() {
			if _, err := svc.Search().LiveRevision(ctx, "main", p, tip); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("AppendOneCommit", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			h := writeRawCommit(b, svc, []string{tip}, map[string]string{p: testFactBody(fmt.Sprintf("x%d", i), 0.5, nil), "kb/noise.md": "n"}, t0.Add(time.Duration(n+i)*time.Minute), "append")
			moveBranch(b, svc, "main", h)
			b.StartTimer()
			if err := svc.rh.AppendCommitLog(ctx, "main", h); err != nil {
				b.Fatal(err)
			}
			tip = h
		}
	})
}

func fullHistoryB(b *testing.B, svc *Service, branch, path, anchor string) []FactRevision {
	b.Helper()
	var out []FactRevision
	var cur *PathHistoryCursor
	for {
		page, next, err := svc.Search().PathHistory(context.Background(), branch, path, anchor, cur, 2)
		require.NoError(b, err)
		out = append(out, page...)
		if next == nil {
			return out
		}
		cur = next
	}
}

func historyCommitsB(revs []FactRevision) []string { return historyCommits(revs) }

// TestLiveRevision_EqualsRevisionsBefore pins LiveRevision to the walk it
// replaces in explain: RevisionsBefore(anchor, 1), at every commit of a
// merge-delivered history, on both branches, including an off-branch anchor
// and a case-only rename.
func TestLiveRevision_EqualsRevisionsBefore(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"
	_, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("v0", 0.5, nil), "create", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))
	for k := range 3 {
		for j := range 2 {
			_, err := svc.Facts().WriteFact(ctx, "agent", p, testFactBody(fmt.Sprintf("%d-%d", k, j), 0.5, nil), "w", "")
			require.NoError(t, err)
		}
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/o%d.md", k), testFactBody("o", 0.5, nil), "o", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
		require.NoError(t, svc.Branches().MergeBranch(ctx, "main", "agent", StrategyLocalWins))
	}
	var all []string
	rows, err := svc.rh.db.Query(`SELECT DISTINCT commit_hash FROM branch_commits`)
	require.NoError(t, err)
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		all = append(all, h)
	}
	rows.Close()
	for _, br := range []string{"main", "agent"} {
		for _, c := range all {
			want := ""
			if revs, err := svc.Search().RevisionsBefore(ctx, br, p, c, 1); err == nil && len(revs) > 0 {
				want = revs[0].Commit
			}
			got, err := svc.Search().LiveRevision(ctx, br, p, c)
			require.NoError(t, err)
			require.Equal(t, want, got, "branch %s anchor %s", br, c)
		}
	}

	// A case-only rename is a commit_log row; LiveRevision sees it too.
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	a := rawCommit(t, svc, "main", []string{tip}, map[string]string{"kb/Case.md": testFactBody("c", 0.5, nil)}, t0, "add")
	b := rawCommit(t, svc, "main", []string{a}, map[string]string{"kb/case.md": testFactBody("c", 0.5, nil)}, t0.Add(time.Hour), "case-only rename")
	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/case.md", b, 1)
	require.NoError(t, err)
	got, err := svc.Search().LiveRevision(ctx, "main", "kb/case.md", b)
	require.NoError(t, err)
	require.Equal(t, revs[0].Commit, got)
	require.Equal(t, []string{a}, historyCommits(fullHistory(t, svc, "main", "kb/case.md", b)), "the rename is not a content change")
}

// TestResolveActiveCommitForPath_FastPathEqualsWalk pins the jump-pointer
// fast path of resolveActiveCommitForPath to the first-parent walk it
// replaces, at every indexed commit, for a merge-delivered history with a
// retraction, a re-add, and a case-only rename.
func TestResolveActiveCommitForPath_FastPathEqualsWalk(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"
	_, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("v0", 0.5, nil), "create", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))
	for k := range 3 {
		_, err := svc.Facts().WriteFact(ctx, "agent", p, testFactBody(fmt.Sprintf("a%d", k), 0.5, nil), "w", "")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/o%d.md", k), testFactBody("o", 0.5, nil), "o", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
		require.NoError(t, svc.Branches().MergeBranch(ctx, "main", "agent", StrategyLocalWins))
	}
	_, err = svc.Facts().DeleteFact(ctx, "main", p, "retract")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", p, testFactBody("back", 0.5, nil), "re-add", "")
	require.NoError(t, err)
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	a := rawCommit(t, svc, "main", []string{tip}, map[string]string{"kb/Case.md": testFactBody("c", 0.5, nil)}, t0, "add")
	rawCommit(t, svc, "main", []string{a}, map[string]string{"kb/case.md": testFactBody("c", 0.5, nil)}, t0.Add(time.Hour), "case-only rename")

	rows, err := svc.rh.db.Query(`SELECT DISTINCT commit_hash FROM branch_commits`)
	require.NoError(t, err)
	var all []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		all = append(all, h)
	}
	rows.Close()
	for _, path := range []string{p, "kb/case.md", "kb/o1.md"} {
		for _, c := range all {
			want, wok, err := svc.rh.resolveActiveCommitForPathWalk(ctx, "main", path, c)
			require.NoError(t, err)
			got, gok, err := svc.rh.resolveActiveCommitForPath(ctx, "main", path, c)
			require.NoError(t, err)
			require.Equal(t, wok, gok, "%s at %s", path, c)
			require.Equal(t, want, got, "%s at %s", path, c)
		}
	}
}
