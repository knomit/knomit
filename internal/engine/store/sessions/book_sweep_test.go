package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newBookIndex(t *testing.T) *ToolIndex {
	t.Helper()
	db, path, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewToolIndex(db, BooksDirFor(path))
}

func newBook(t *testing.T, ti *ToolIndex, owner string) string {
	t.Helper()
	id, err := ti.CreateBook(context.Background(), BookToolPrefix+"3", "b", "repo:x", "rs", owner, "{}",
		[]BookResult{{Unit: 1, Path: "kb/a.md", Anchor: "f-a"}})
	require.NoError(t, err)
	return id
}

func setLastUsed(t *testing.T, ti *ToolIndex, id string, at time.Time) {
	t.Helper()
	_, err := ti.db.Exec(`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339), id)
	require.NoError(t, err)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func policy(now time.Time) BookSweepPolicy {
	return BookSweepPolicy{Now: now, IdleTTL: 15 * time.Minute, Grace: 5 * time.Minute}
}

// Startup: Open recreates the book directory empty.
//
// Sabotage: drop the RemoveAll in Open, and a book file of the last run
// survives.
func TestBookFiles_StartupSweepEmptiesTheDirectory(t *testing.T) {
	main := filepath.Join(t.TempDir(), "k.db")
	db, path, err := Open(main)
	require.NoError(t, err)
	ti := NewToolIndex(db, BooksDirFor(path))
	id := newBook(t, ti, "o")
	require.True(t, exists(ti.bookPath(id)))
	require.NoError(t, db.Close())
	db, _, err = Open(main)
	require.NoError(t, err)
	defer db.Close()
	require.False(t, exists(ti.bookPath(id)), "the previous run's book file is gone")
}

// Expired: a book whose cursor was last used before now - idle TTL - grace
// loses its row and its file, even before the reaper ran; a younger one
// stays.
//
// Sabotage: compare with IdleTTL alone (no grace), and the 17-minute-old
// book is swept.
func TestBookFiles_ExpiredAreDeleted(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	old, young := newBook(t, ti, "a"), newBook(t, ti, "b")
	setLastUsed(t, ti, old, now.Add(-21*time.Minute))
	setLastUsed(t, ti, young, now.Add(-17*time.Minute))
	st, err := ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Expired)
	require.False(t, exists(ti.bookPath(old)))
	info, err := ti.GetBook(context.Background(), old)
	require.NoError(t, err)
	require.Nil(t, info, "its cursor row went with it")
	require.True(t, exists(ti.bookPath(young)), "inside idle TTL + grace it stays")
}

// Active: a file a call holds open is never deleted, however old its
// cursor looks, and is swept once released.
//
// Sabotage: drop the openBooks check, and the open file is deleted.
func TestBookFiles_OpenFilesAreKept(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	id := newBook(t, ti, "a")
	setLastUsed(t, ti, id, now.Add(-time.Hour))
	f, ok, err := ti.OpenBookFile(id)
	require.NoError(t, err)
	require.True(t, ok)
	st, err := ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Skipped)
	require.True(t, exists(ti.bookPath(id)), "a page in flight keeps its file")
	_, err = f.Results(context.Background())
	require.NoError(t, err, "and can still read it")
	require.NoError(t, f.Close())
	st, err = ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Expired)
	require.False(t, exists(ti.bookPath(id)))
}

// Orphans: a book file whose cursor row is gone (the reaper deleted it) is
// deleted; and sidecars and stray files with no live book go too, but a
// stray file younger than the grace stays.
//
// Sabotage: skip files with no row (no orphan delete), or keep every
// non-.db file.
func TestBookFiles_OrphansAndSidecarsAreDeleted(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	ctx := context.Background()
	live, gone := newBook(t, ti, "a"), newBook(t, ti, "b")
	_, err := ti.db.Exec(`DELETE FROM tool_sessions WHERE id = ?`, gone)
	require.NoError(t, err)
	stray := filepath.Join(ti.booksDir, "dead.db-journal")
	liveSide := ti.bookPath(live) + "-journal"
	partial := filepath.Join(ti.booksDir, "tmp-partial")
	fresh := filepath.Join(ti.booksDir, "tmp-fresh")
	for _, p := range []string{stray, liveSide, partial, fresh} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	require.NoError(t, os.Chtimes(partial, now.Add(-time.Hour), now.Add(-time.Hour)))

	st, err := ti.SweepBookFiles(ctx, policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Orphans)
	require.False(t, exists(ti.bookPath(gone)), "the orphan is deleted")
	require.False(t, exists(stray), "a sidecar of no book is deleted")
	require.False(t, exists(partial), "an old stray file is deleted")
	require.True(t, exists(fresh), "a stray file younger than the grace stays")
	require.True(t, exists(liveSide), "a live book's sidecar stays")
	require.True(t, exists(ti.bookPath(live)))
}

// Size cap: over MaxBytes the book idle longest goes first, row and file,
// until the directory fits.
//
// Sabotage: sort newest first, and the most recently used book is evicted.
func TestBookFiles_SizeCapEvictsOldestFirst(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	ctx := context.Background()
	a, b, c := newBook(t, ti, "a"), newBook(t, ti, "b"), newBook(t, ti, "c")
	setLastUsed(t, ti, a, now.Add(-3*time.Minute))
	setLastUsed(t, ti, b, now.Add(-1*time.Minute))
	setLastUsed(t, ti, c, now.Add(-2*time.Minute))
	fi, err := os.Stat(ti.bookPath(a))
	require.NoError(t, err)
	p := policy(now)
	p.MaxBytes = 2 * fi.Size()
	st, err := ti.SweepBookFiles(ctx, p)
	require.NoError(t, err)
	require.Equal(t, 1, st.Evicted)
	require.False(t, exists(ti.bookPath(a)), "the book idle longest is evicted")
	info, err := ti.GetBook(ctx, a)
	require.NoError(t, err)
	require.Nil(t, info, "its cursor is gone too: it reads as expired")
	require.True(t, exists(ti.bookPath(b)))
	require.True(t, exists(ti.bookPath(c)))
}
