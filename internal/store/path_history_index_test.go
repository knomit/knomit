package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	storegit "knomit/internal/store/git"
)

// Tests for derive-at-index (review rounds 3-4): a commit's path_changes rows
// are committed no later than the transaction that records it, derivation
// depends on git alone, and derived rows are never deleted.

// TestPathHistory_TipFirstIndexingNeverTruncates is the regression for review
// N1: a commit derived before its parent was indexed was marked done with an
// unresolved link, and its history stayed [C] instead of [C, P, base] for
// good. Now appending a tip whose ancestors were never indexed derives the
// ancestors first (from git), and the recording hook refuses a commit that is
// not derived instead of making it visible without its history.
func TestPathHistory_TipFirstIndexingNeverTruncates(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := writeRawCommit(t, svc, nil, map[string]string{p: testFactBody("base", 0.5, nil)}, t0, "base")
	parent := writeRawCommit(t, svc, []string{base}, map[string]string{p: testFactBody("P", 0.6, nil)}, t0.Add(time.Hour), "P")
	tip := writeRawCommit(t, svc, []string{parent}, map[string]string{p: testFactBody("C", 0.7, nil)}, t0.Add(2*time.Hour), "C")
	moveBranch(t, svc, "main", tip)

	// Recording a commit that is not derived is refused, and nothing lands.
	d := newDeriver(svc.rh, activeTables)
	err := svc.rh.gits.CommitLogApply(ctx, "main", []storegit.CommitLogItem{{Hash: tip, Parents: []string{parent}}},
		storegit.CommitLogApplyOptions{Derive: d.hook([]string{tip}, nil)})
	require.ErrorIs(t, err, errNotDerived)
	var visible int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_commits WHERE commit_hash = ?`, tip).Scan(&visible))
	require.Zero(t, visible, "the refused commit is not visible: its transaction rolled back")

	// The append path derives the never-indexed ancestors first.
	require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", tip))
	require.Equal(t, []string{tip, parent, base}, historyCommits(fullHistory(t, svc, "main", p, tip)))
}

// reopen closes svc and opens the same database again, as a restart does.
func reopen(t *testing.T, svc *Service, dbPath string) *Service {
	t.Helper()
	require.NoError(t, svc.Close())
	svc, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	return svc
}

func clearDerived(t *testing.T, svc *Service, version string) {
	t.Helper()
	for _, stmt := range []string{`DELETE FROM path_changes`, `DELETE FROM path_change_links`, `DELETE FROM commit_fp`, `DELETE FROM meta WHERE key = 'path_changes_version'`} {
		_, err := svc.rh.db.Exec(stmt)
		require.NoError(t, err)
	}
	if version != "" {
		_, err := svc.rh.db.Exec(`INSERT INTO meta(key, value) VALUES ('path_changes_version', ?)`, version)
		require.NoError(t, err)
	}
}

// TestPathHistory_LegacyGapDerivesFromGit is the regression for review R1
// (a blocker): an indexed commit whose parent is in git but on no
// branch_commits row made the one-time pass fail, and every later populate
// and write with it. Derivation now walks git, so open, a later write and the
// history all succeed — and populate records the gap.
func TestPathHistory_LegacyGapDerivesFromGit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	var commits []string
	for i := range 5 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	_, err = svc.rh.db.Exec(`DELETE FROM branch_commits WHERE commit_hash = ?`, commits[2])
	require.NoError(t, err)
	clearDerived(t, svc, "")

	svc = reopen(t, svc, dbPath)
	require.Equal(t, reversed(commits), historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[4])))
	r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("after", 0.5, nil), "after", "")
	require.NoError(t, err, "writes keep working")
	require.Equal(t, reversed(append(commits, r.CommitHash)), historyCommits(fullHistory(t, svc, "main", "kb/t.md", r.CommitHash)))
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_commits WHERE commit_hash = ?`, commits[2]).Scan(&n))
	require.Equal(t, 1, n, "populate recorded the gap")
}

// TestPathHistory_VersionBumpOverDeletedSideBranch: a version bump over a
// merge whose side commits were indexed only on a since-deleted branch. The
// reset and re-derivation run from git over every branch tip, so main's
// history still contains the side's writes.
func TestPathHistory_VersionBumpOverDeletedSideBranch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	base, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("base", 0.5, nil), "base", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "side", "main"))
	var sides []string
	for i := range 3 {
		r, err := svc.Facts().WriteFact(ctx, "side", "kb/t.md", testFactBody(fmt.Sprintf("s%d", i), 0.5, nil), "s", "")
		require.NoError(t, err)
		sides = append(sides, r.CommitHash)
	}
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/other.md", testFactBody("o", 0.5, nil), "o", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().MergeBranch(ctx, "side", "main", StrategyLocalWins))
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	want := append(reversed(sides), base.CommitHash)
	require.Equal(t, want, historyCommits(fullHistory(t, svc, "main", "kb/t.md", tip)))

	// Legacy shape: the side commits were only ever indexed on "side".
	for _, h := range sides {
		_, err := svc.rh.db.Exec(`DELETE FROM branch_commits WHERE commit_hash = ? AND branch_id = (SELECT id FROM branches WHERE name = 'main')`, h)
		require.NoError(t, err)
	}
	require.NoError(t, svc.Branches().DropBranch(ctx, "side"))
	clearDerived(t, svc, "0") // a STALE version: the reset path

	svc = reopen(t, svc, dbPath)
	require.Equal(t, want, historyCommits(fullHistory(t, svc, "main", "kb/t.md", tip)))
}

// TestPathHistory_ReadErrorRecordsNothing is the regression for review R3: a
// git read error during derivation was taken for absence and recorded for
// good. Now it fails the derivation, nothing is recorded or made visible, and
// the next attempt derives the full history.
func TestPathHistory_ReadErrorRecordsNothing(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var commits []string
	for i := range 3 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	next := writeRawCommit(t, svc, []string{commits[2]}, map[string]string{"kb/t.md": testFactBody("v3", 0.6, nil)}, t0, "v3")
	moveBranch(t, svc, "main", next)
	prevBlob := plumbing.NewHash(blobOf(t, svc, commits[2], "kb/t.md"))
	injected := errors.New("injected read error")
	deriveObjectHook = func(kind string, h plumbing.Hash) error {
		if kind == "blob" && h == prevBlob {
			return injected
		}
		return nil
	}
	err := svc.rh.AppendCommitLog(ctx, "main", next)
	deriveObjectHook = nil
	require.ErrorIs(t, err, injected)
	for _, table := range []string{"branch_commits", "commit_fp", "path_changes"} {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE commit_hash = ?`, next).Scan(&n))
		require.Zero(t, n, "%s: nothing recorded for the failed commit", table)
	}

	require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", next))
	require.Equal(t, reversed(append(commits, next)), historyCommits(fullHistory(t, svc, "main", "kb/t.md", next)))
}

// pauseSwap makes the next rewind/rebuild swap run check inside its
// transaction, after it cleared the branch and before it re-records it.
func pauseSwap(t *testing.T, check func()) {
	t.Helper()
	swapHook = func() error { check(); return nil }
	t.Cleanup(func() { swapHook = nil })
}

func branchCommitCount(t *testing.T, svc *Service, branch string) int {
	t.Helper()
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_commits WHERE branch_id = (SELECT id FROM branches WHERE name = ?)`, branch).Scan(&n))
	return n
}

// TestPathHistory_RewindIsAtomic covers review N2/R4 for a rewind: paused in
// the middle of the swap, a reader on another connection sees the whole old
// branch — never an empty or half-recorded one — and afterwards the new one.
func TestPathHistory_RewindIsAtomic(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var commits []string
	for i := range 5 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	oldCount := branchCommitCount(t, svc, "main")
	paused := false
	pauseSwap(t, func() {
		paused = true
		require.Equal(t, oldCount, branchCommitCount(t, svc, "main"), "mid-swap the reader sees the old branch, whole")
		page, _, err := svc.Search().PathHistory(context.Background(), "main", "kb/t.md", commits[4], nil, 10)
		require.NoError(t, err)
		require.Len(t, page, 5)
	})
	moveBranch(t, svc, "main", commits[2])
	require.NoError(t, svc.rh.repopulateBranch(ctx, "main"))
	require.True(t, paused, "the swap ran through the pause")
	require.Equal(t, oldCount-2, branchCommitCount(t, svc, "main"))
	require.Equal(t, reversed(commits[:3]), historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[2])))
}

// TestPathHistory_RebuildIsAtomicAndKeepsOtherBranches covers review R2/R4/R5:
// paused in the middle of the swap, a reader sees the old branch whole; after
// the rebuild the history is unchanged, rows a tampering removed are derived
// again, and another branch sharing commits keeps its history.
func TestPathHistory_RebuildIsAtomicAndKeepsOtherBranches(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var commits []string
	for i := range 4 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	require.NoError(t, svc.Branches().CreateBranch(ctx, "other", "main"))
	o, err := svc.Facts().WriteFact(ctx, "other", "kb/t.md", testFactBody("other", 0.5, nil), "o", "")
	require.NoError(t, err)
	wantMain := historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[3]))
	wantOther := historyCommits(fullHistory(t, svc, "other", "kb/t.md", o.CommitHash))

	// Missing rows (not deleted by anything here: removed to show rebuild
	// derives what is missing).
	_, err = svc.rh.db.Exec(`DELETE FROM commit_fp WHERE commit_hash = ?`, commits[1])
	require.NoError(t, err)
	_, err = svc.rh.db.Exec(`DELETE FROM path_changes WHERE commit_hash = ?`, commits[1])
	require.NoError(t, err)

	oldCount := branchCommitCount(t, svc, "main")
	paused := false
	pauseSwap(t, func() {
		paused = true
		require.Equal(t, oldCount, branchCommitCount(t, svc, "main"), "mid-rebuild the reader sees the old branch, whole")
	})
	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.True(t, paused)
	require.Equal(t, wantMain, historyCommits(fullHistory(t, svc, "main", "kb/t.md", commits[3])))
	require.Equal(t, wantOther, historyCommits(fullHistory(t, svc, "other", "kb/t.md", o.CommitHash)), "another branch's history survives")
}

// TestPathHistory_RebuildDoesNotBlockWriters covers review R2: a writer on
// another branch during a rebuild never sees "database is locked" — the
// derivation runs in short batches and the swap is one short transaction.
func TestPathHistory_RebuildDoesNotBlockWriters(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var prev []string
	var tip string
	for i := range 2000 {
		tip = writeRawCommit(t, svc, prev, map[string]string{"kb/t.md": testFactBody(fmt.Sprintf("v%d", i/50), 0.5, nil), "kb/n.md": testFactBody(fmt.Sprintf("n%d", i), 0.5, nil)}, t0.Add(time.Duration(i)*time.Minute), "c")
		prev = []string{tip}
	}
	moveBranch(t, svc, "main", tip)
	require.NoError(t, svc.rh.populateCommitLog(ctx, "main"))
	require.NoError(t, svc.Branches().CreateBranch(ctx, "writer", "main"))
	// Force the rebuild to derive everything again, in batches.
	clearDerived(t, svc, pathChangesVersion)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var worst time.Duration
	var werr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s := time.Now()
			_, err := svc.Facts().WriteFact(ctx, "writer", fmt.Sprintf("kb/w%d.md", i), testFactBody("w", 0.5, nil), "w", "")
			if d := time.Since(s); d > worst {
				worst = d
			}
			if err != nil {
				werr = err
				return
			}
		}
	}()
	start := time.Now()
	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	require.NoError(t, werr, "a concurrent writer never fails with a lock timeout")
	swap := time.Duration(lastSwapDuration.Load())
	t.Logf("rebuild %v (re-deriving 2000 commits); swap transaction %v; worst concurrent write %v", elapsed, swap, worst)
	require.Less(t, worst, 5*time.Second)
	// The lock is held only for the swap: re-deriving 2000 commits happens
	// before it, in short batches. A rebuild that derived inside the swap
	// would hold it for most of the run.
	require.Less(t, swap, elapsed/4, "the swap transaction is a small part of the rebuild")
}

// TestPathHistory_IndexedButUnderivedIsAnError: indexing guarantees every
// recorded commit is derived, so this state is unreachable except by damage;
// the defensive error still turns it into a failure instead of an empty
// first page.
func TestPathHistory_IndexedButUnderivedIsAnError(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v", 0.5, nil), "v", "")
	require.NoError(t, err)
	_, err = svc.rh.db.Exec(`DELETE FROM commit_fp WHERE commit_hash = ?`, r.CommitHash)
	require.NoError(t, err)
	_, _, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", r.CommitHash, nil, 3)
	require.ErrorContains(t, err, "never derived")
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
	// The same append recorded without derivation (CommitLogApply with no
	// hook — dev's cost) on its own branch, for the per-write overhead.
	require.NoError(b, svc.Branches().CreateBranch(ctx, "nodrv", "main"))
	nodrvTip := tip
	b.Run("AppendOneCommitWithoutDerive", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			h := writeRawCommit(b, svc, []string{nodrvTip}, map[string]string{p: testFactBody(fmt.Sprintf("y%d", i), 0.5, nil), "kb/noise.md": "n"}, t0.Add(time.Duration(n+i)*time.Minute), "append")
			moveBranch(b, svc, "nodrv", h)
			b.StartTimer()
			c, err := svc.rh.repo.CommitObject(plumbing.NewHash(h))
			if err != nil {
				b.Fatal(err)
			}
			items, err := svc.rh.indexItems(ctx, newDeriver(svc.rh, activeTables), []*object.Commit{c}, false)
			if err != nil {
				b.Fatal(err)
			}
			noDerive := func(context.Context, *sql.Tx, int) error { return nil }
			if err := svc.rh.gits.CommitLogApply(ctx, "nodrv", items, storegit.CommitLogApplyOptions{Derive: noDerive}); err != nil {
				b.Fatal(err)
			}
			nodrvTip = h
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
	// Rebuild of the 10k branch with everything derived (the common case),
	// and the worst wait of a concurrent writer on another branch.
	require.NoError(b, svc.Branches().CreateBranch(ctx, "writer", "main"))
	// Baseline: the same writer with no rebuild running.
	var base time.Duration
	for i := range 20 {
		s := time.Now()
		_, err := svc.Facts().WriteFact(ctx, "writer", fmt.Sprintf("kb/base%d.md", i), testFactBody("w", 0.5, nil), "w", "")
		require.NoError(b, err)
		base = max(base, time.Since(s))
	}
	var worst time.Duration
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s := time.Now()
			if _, err := svc.Facts().WriteFact(ctx, "writer", fmt.Sprintf("kb/w%d.md", i), testFactBody("w", 0.5, nil), "w", ""); err != nil {
				b.Error(err)
				return
			}
			worst = max(worst, time.Since(s))
		}
	}()
	start = time.Now()
	require.NoError(b, svc.rh.rebuildCommitLog(ctx, "main"))
	elapsed := time.Since(start)
	close(stop)
	<-done
	b.Logf("rebuild of %d commits: %v; swap transaction %v; worst concurrent write %v (worst of 20 writes with no rebuild: %v)", n, elapsed, time.Duration(lastSwapDuration.Load()), worst, base)
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
