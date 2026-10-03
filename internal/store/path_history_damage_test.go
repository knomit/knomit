package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// Round-5 tests: derivation from git alone, missing objects as boundaries or
// content_unavailable (never a failed write), re-derivation once objects are
// back.

func writeVersionsStore(t *testing.T, svc *Service, n int) []string {
	t.Helper()
	ctx := context.Background()
	var commits []string
	for i := range n {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5+float64(i)/100, nil), fmt.Sprintf("v%d", i), "")
		require.NoError(t, err)
		commits = append(commits, r.CommitHash)
	}
	return commits
}

// removeObject deletes a git object and returns a function that puts it back.
func removeObject(t *testing.T, svc *Service, h plumbing.Hash) (restore func()) {
	t.Helper()
	obj, err := svc.rh.gits.EncodedObject(plumbing.AnyObject, h)
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.DeleteObjectForTest(h))
	return func() {
		_, err := svc.rh.gits.SetEncodedObject(obj)
		require.NoError(t, err)
	}
}

func staleVersion(t *testing.T, svc *Service) {
	t.Helper()
	_, err := svc.rh.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES ('path_changes_version', '0')`)
	require.NoError(t, err)
}

// TestPathHistory_TamperedCommitLogIsIgnored is the regression for review A1:
// derivation followed commit_log instead of git, so a commit_log row set to
// 'deleted' lost c2 from the history for good. It now always diffs the trees.
func TestPathHistory_TamperedCommitLogIsIgnored(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c := writeVersionsStore(t, svc, 3)
	_, err := svc.rh.db.Exec(`UPDATE commit_log SET action = 'deleted' WHERE commit_hash = ? AND path = 'kb/t.md'`, c[1])
	require.NoError(t, err)
	staleVersion(t, svc)
	require.NoError(t, svc.rh.openHistory(ctx))
	require.Equal(t, reversed(c), historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[2])))
	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Equal(t, reversed(c), historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[2])))
}

// TestPathHistory_MissingBlobIsContentUnavailable is the regression for review
// A2: one unreadable blob failed the whole derivation, and with the history
// underived every write failed after its ref advanced. Now a missing blob
// marks the revision (and the one edited from it) content_unavailable; open,
// writes and the history all succeed.
func TestPathHistory_MissingBlobIsContentUnavailable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	c := writeVersionsStore(t, svc, 3)
	removeObject(t, svc, plumbing.NewHash(blobOf(t, svc, c[0], "kb/t.md")))
	clearDerived(t, svc, "") // an upgraded database

	svc = reopen(t, svc, dbPath)
	revs := fullHistory(t, svc, "main", "kb/t.md", c[2])
	require.Equal(t, reversed(c), historyCommits(revs))
	byCommit := map[string]FactRevision{}
	for _, r := range revs {
		byCommit[r.Commit] = r
	}
	require.True(t, byCommit[c[0]].ContentUnavailable, "the revision whose blob is missing")
	require.True(t, byCommit[c[1]].ContentUnavailable, "the revision edited from it has no diff base")
	require.Nil(t, byCommit[c[1]].Diff)
	require.False(t, byCommit[c[2]].ContentUnavailable)
	require.NotNil(t, byCommit[c[2]].Diff)

	r, err := svc.Facts().WriteFact(context.Background(), "main", "kb/t.md", testFactBody("after", 0.9, nil), "after", "")
	require.NoError(t, err, "writes keep working")
	require.Equal(t, reversed(append(c, r.CommitHash)), historyCommits(fullHistory(t, svc, "main", "kb/t.md", r.CommitHash)))
}

// TestPathHistory_RestoredObjectsAreRederived covers review A5: rows derived
// around a missing object are not frozen — once the object is back, :rebuild
// re-derives them. Both a missing blob and a missing commit (a boundary).
func TestPathHistory_RestoredObjectsAreRederived(t *testing.T) {
	t.Run("blob", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		c := writeVersionsStore(t, svc, 3)
		restore := removeObject(t, svc, plumbing.NewHash(blobOf(t, svc, c[0], "kb/t.md")))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.openHistory(ctx))
		require.True(t, fullHistory(t, svc, "main", "kb/t.md", c[2])[2].ContentUnavailable)

		restore()
		require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
		revs := fullHistory(t, svc, "main", "kb/t.md", c[2])
		for _, r := range revs {
			require.False(t, r.ContentUnavailable, "%s re-derived", r.Commit)
		}
		require.NotNil(t, revs[1].Diff, "the diff against the restored blob is back")
	})
	t.Run("commit", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		c := writeVersionsStore(t, svc, 5)
		restore := removeObject(t, svc, plumbing.NewHash(c[1]))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.openHistory(ctx))
		revs := fullHistory(t, svc, "main", "kb/t.md", c[4])
		require.Equal(t, []string{c[4], c[3], c[2]}, historyCommits(revs), "history starts at the boundary")
		require.Equal(t, "added", revs[2].Action)

		restore()
		require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
		require.Equal(t, reversed(c), historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[4])))
	})
}

// TestPathHistory_TreeAndParentReadErrorsFail covers review R3 for the tree
// read and the parent-commit read: a transient error fails the derivation
// before the ref moves; nothing is recorded; a retry derives it.
func TestPathHistory_TreeAndParentReadErrorsFail(t *testing.T) {
	for _, kind := range []string{"tree", "commit"} {
		t.Run(kind, func(t *testing.T) {
			svc, ctx := openPathHistoryStore(t)
			c := writeVersionsStore(t, svc, 2)
			t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			next := writeRawCommit(t, svc, []string{c[1]}, map[string]string{"kb/t.md": testFactBody("v2", 0.6, nil)}, t0, "v2")
			parent, err := svc.rh.repo.CommitObject(plumbing.NewHash(c[1]))
			require.NoError(t, err)
			target := parent.TreeHash // the parent's tree: read by the diff and by blobAt
			if kind == "commit" {
				target = parent.Hash
			}
			injected := errors.New("injected " + kind + " read error")
			deriveObjectHook = func(k string, h plumbing.Hash) error {
				if k == kind && h == target {
					return injected
				}
				return nil
			}
			err = svc.rh.deriveBeforeAdvance(ctx, plumbing.NewHash(next))
			deriveObjectHook = nil
			require.ErrorIs(t, err, injected)
			derived, err := newDeriver(svc.rh, activeTables).isDerived(ctx, svc.rh.db, next)
			require.NoError(t, err)
			require.False(t, derived, "nothing recorded")

			require.NoError(t, svc.rh.deriveBeforeAdvance(ctx, plumbing.NewHash(next)))
			moveBranch(t, svc, "main", next)
			require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", next))
			require.Equal(t, reversed(append(c, next)), historyCommits(fullHistory(t, svc, "main", "kb/t.md", next)))
		})
	}
}

// TestDeriver_ApplyNeverReadsGit pins apply's contract: a git read while
// applying is an error, so any derivation test fails if apply ever reads one.
func TestDeriver_ApplyNeverReadsGit(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	c := writeVersionsStore(t, svc, 1)
	d := newDeriver(svc.rh, activeTables)
	d.applying = true
	_, err := d.commit(plumbing.NewHash(c[0]))
	require.ErrorIs(t, err, errReadInApply)
}

// TestLiveRevision_EqualsRevisionsBeforeEdgeCases extends the equivalence to a
// composing merge, a lower -> Upper rename, and a history boundary (above it
// the two agree; at an anchor whose version is below it, LiveRevision finds
// nothing, as the history does).
func TestLiveRevision_EqualsRevisionsBeforeEdgeCases(t *testing.T) {
	check := func(t *testing.T, svc *Service, path string, anchors []string) {
		t.Helper()
		for _, a := range anchors {
			want := ""
			if revs, err := svc.Search().RevisionsBefore(context.Background(), "main", path, a, 1); err == nil && len(revs) > 0 {
				want = revs[0].Commit
			}
			got, err := svc.Search().LiveRevision(context.Background(), "main", path, a)
			require.NoError(t, err)
			require.Equal(t, want, got, "%s at %s", path, a)
		}
	}
	t.Run("composing merge and case rename", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		const p = "kb/t.md"
		b, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("B", 0.5, nil), "B", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().CreateBranch(ctx, "side", "main"))
		x, err := svc.Facts().WriteFact(ctx, "side", p, testFactBody("B", 0.7, nil), "X", "")
		require.NoError(t, err)
		y, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("B edited", 0.5, nil), "Y", "")
		require.NoError(t, err)
		_, err = svc.rh.mergeIntoBranchResolved(ctx, "side", "main", StrategyRefuse, map[string]Resolution{p: {Body: []byte(testFactBody("B edited", 0.7, nil))}})
		require.NoError(t, err)
		m, err := svc.Branches().HeadCommit(ctx, "main")
		require.NoError(t, err)
		check(t, svc, p, []string{b.CommitHash, x.CommitHash, y.CommitHash, m})

		t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		lo := rawCommit(t, svc, "main", []string{m}, map[string]string{"kb/lc.md": testFactBody("c", 0.5, nil)}, t0, "lower")
		up := rawCommit(t, svc, "main", []string{lo}, map[string]string{"kb/LC.md": testFactBody("c", 0.5, nil)}, t0.Add(time.Hour), "lower -> Upper")
		check(t, svc, "kb/lc.md", []string{lo, up})
	})
	t.Run("boundary", func(t *testing.T) {
		svc, ctx := openPathHistoryStore(t)
		c := writeVersionsStore(t, svc, 5)
		removeObject(t, svc, plumbing.NewHash(c[1]))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.openHistory(ctx))
		check(t, svc, "kb/t.md", c[2:]) // versions above the boundary
		got, err := svc.Search().LiveRevision(ctx, "main", "kb/t.md", c[0])
		require.NoError(t, err)
		require.Empty(t, got, "below the boundary nothing is derived")
	})
}
