package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/internal/vcs"
)

type pathChangeRow struct {
	blob, diff  string
	unavailable int
}

func readPathChange(t *testing.T, svc *Service, path, commit string) pathChangeRow {
	t.Helper()
	var r pathChangeRow
	require.NoError(t, svc.rh.DB().QueryRow(`SELECT blob, COALESCE(diff, ''), content_unavailable FROM path_changes
		WHERE path = ? AND commit_hash = ? AND entry = 1`, path, commit).Scan(&r.blob, &r.diff, &r.unavailable))
	return r
}

// NB1 (follow-up to #698): a degraded = 1 commit whose re-derivation would
// cut the line (its first parent's commit object is now missing) keeps its
// depth and jump pointers — but its CONTENT is still refreshed once its blob
// is back: the path_changes row gets the blob and the diff a clean
// derivation stored, content_unavailable clears, and the commit's degraded
// mark drops to 0. While the blob is still missing nothing changes.
//
// Fixture: o writes kb/p.md, P is filler (A's first parent), A modifies
// kb/p.md, three commits above. The clean row of A is recorded first; A's
// blob is then removed and the history re-derived (A content_unavailable,
// degraded 1); then P's commit object goes missing and the re-derivation pass
// runs.
//
// Sabotage: keep the row without applyContentRefresh (the #698 behaviour),
// and A's revision stays content_unavailable with an empty blob.
func TestRederive_KeptRowStillRefreshesContent(t *testing.T) {
	setup := func(t *testing.T) (svc *Service, p, a, tip string, clean pathChangeRow, restoreBlob func()) {
		svc, ctx := openPathHistoryStore(t)
		writeMergeFact(t, svc, "main", "kb/p.md", "p", "p one")
		p = writeMergeFact(t, svc, "main", "kb/f.md", "f", "P")
		a = writeMergeFact(t, svc, "main", "kb/p.md", "p", "p two, at A")
		for i := 0; i < 3; i++ {
			tip = writeMergeFact(t, svc, "main", fmt.Sprintf("kb/d%d.md", i), "d", "above")
		}
		clean = readPathChange(t, svc, "kb/p.md", a)
		require.Equal(t, 0, clean.unavailable, "precondition: a clean row")
		require.NotEmpty(t, clean.blob)
		require.NotEmpty(t, clean.diff, "precondition: A's row carries a diff against o")
		restoreBlob = removeObject(t, svc, plumbing.NewHash(clean.blob))
		staleVersion(t, svc)
		require.NoError(t, svc.rh.OpenHistory(ctx))
		dmg := readPathChange(t, svc, "kb/p.md", a)
		require.Equal(t, 1, dmg.unavailable, "precondition: A's revision is content_unavailable")
		_, deg, _, _ := fpRow(t, svc, a)
		require.Equal(t, 1, deg, "precondition: A is degraded = 1")
		return svc, p, a, tip, clean, restoreBlob
	}
	t.Run("blob back, parent missing: content refreshed, structure kept", func(t *testing.T) {
		svc, p, a, tip, clean, restoreBlob := setup(t)
		ctx := context.Background()
		depth, _, ups, _ := fpRow(t, svc, a)
		restoreBlob()
		removeObject(t, svc, plumbing.NewHash(p))
		_, err := svc.si.RepoForTest().RederiveDegradedForTest(ctx)
		require.NoError(t, err)

		d, deg, u, ok := fpRow(t, svc, a)
		require.True(t, ok)
		require.Equal(t, depth, d, "depth kept")
		require.Equal(t, ups, u, "jump pointers kept")
		require.Equal(t, 0, deg, "nothing is missing any more: degraded 0")
		require.Equal(t, clean, readPathChange(t, svc, "kb/p.md", a), "the row a clean derivation stored")
		var links int
		require.NoError(t, svc.rh.DB().QueryRow(`SELECT count(*) FROM path_change_links WHERE path = 'kb/p.md' AND commit_hash = ?`, a).Scan(&links))
		require.Equal(t, 1, links, "the edited-from link is kept")
		for _, r := range fullHistory(t, svc, "main", "kb/p.md", tip) {
			require.False(t, r.ContentUnavailable, "%s available again", r.Commit[:8])
		}
		requireReadersAgreeAt(t, svc, "kb/p.md", tip)
	})
	t.Run("blob still missing: nothing changes", func(t *testing.T) {
		svc, p, a, _, _, _ := setup(t)
		ctx := context.Background()
		before := readPathChange(t, svc, "kb/p.md", a)
		removeObject(t, svc, plumbing.NewHash(p))
		_, err := svc.si.RepoForTest().RederiveDegradedForTest(ctx)
		require.NoError(t, err)
		require.Equal(t, before, readPathChange(t, svc, "kb/p.md", a))
		_, deg, _, _ := fpRow(t, svc, a)
		require.Equal(t, 1, deg, "still degraded = 1")
	})
}

// NB2 (follow-up to #698): when LineEvents' ROLLBACK fails, the connection
// that may still be inside its read transaction is discarded, never pooled.
// A one-connection pool makes that observable: the next BEGIN on the pool
// must succeed (a fresh connection), not fail with "cannot start a
// transaction within a transaction" (the same connection, still in the
// snapshot).
//
// Sabotage: drop the Conn.Raw(driver.ErrBadConn), and the next BEGIN fails.
func TestLineEvents_FailedRollbackDiscardsTheConnection(t *testing.T) {
	svc, dbPath := openPathHistoryStoreAt(t)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	w := writeMergeFact(t, svc, "main", "kb/p.md", "p", "p")
	tip := writeMergeFact(t, svc, "main", "kb/q.md", "q", "q")

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	rollbacks := 0
	restore := vcs.SetLineRollbackForTest(func(ctx context.Context, conn *sql.Conn) error {
		rollbacks++
		return errors.New("injected: rollback failed") // and the transaction stays open
	})
	evs, err := vcs.LineEvents(ctx, db, tip, vcs.LineQuery{Path: "kb/p.md", Writes: true, Limit: 1})
	restore()
	require.NoError(t, err)
	require.Equal(t, 1, rollbacks, "precondition: the failing rollback ran")
	require.Len(t, evs, 1)
	require.Equal(t, w, evs[0].Commit)

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `BEGIN`)
	require.NoError(t, err, "the pool's connection is a fresh one, not the one left in the snapshot")
	_, err = conn.ExecContext(ctx, `ROLLBACK`)
	require.NoError(t, err)
}
