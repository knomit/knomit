package sessions

import (
	"context"
	"fmt"
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
	ti := NewToolIndex(db, BooksDirFor(path))
	t.Cleanup(func() { ti.CloseBooks(); _ = db.Close() })
	return ti
}

func newBook(t *testing.T, ti *ToolIndex, owner string) string {
	t.Helper()
	id, err := ti.CreateBook(context.Background(), BookToolPrefix+"3", "b", "repo:x", "rs", owner, "{}",
		[]BookResult{{Unit: 1, Path: "kb/a.md", Anchor: "f-a"}, {Unit: 2, Path: "kb/b.md", Anchor: "f-b"}})
	require.NoError(t, err)
	return id
}

// appendUnit appends unit with n rows of size bytes of state each.
func appendUnit(t *testing.T, ti *ToolIndex, id string, unit, from, n, size int) {
	t.Helper()
	b, ok := ti.OpenBook(id)
	require.True(t, ok)
	defer b.Close()
	rows := make([]BookRow, n)
	for i := range rows {
		rows[i] = BookRow{Seq: from + i, Unit: unit, N: i + 1, Path: fmt.Sprintf("kb/z/%d-%d.md", unit, i), Written: true,
			State: fmt.Sprintf("%0*d", size, from+i)}
	}
	require.NoError(t, b.AppendUnit(context.Background(), unit, rows, nil, nil, nil))
}

func spill(t *testing.T, ti *ToolIndex, id string) {
	t.Helper()
	ti.booksMu.Lock()
	bs := ti.books[id]
	ti.booksMu.Unlock()
	bs.mu.Lock()
	defer bs.mu.Unlock()
	require.NoError(t, bs.spillLocked(context.Background()))
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

// A small book never touches disk: it is created, appended and paged in
// memory, and no file appears.
//
// Sabotage: create the file in CreateBook (or spill on every append), and a
// file exists.
func TestBooks_SmallBookNeverCreatesAFile(t *testing.T) {
	ti := newBookIndex(t)
	id := newBook(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 50, 400)
	b, ok := ti.OpenBook(id)
	require.True(t, ok)
	rows, err := b.Rows(context.Background(), 1, 5)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	require.False(t, b.Spilled())
	b.Close()
	require.False(t, exists(ti.bookPath(id)), "no file for a small book")
	files, _, err := ti.BookFiles()
	require.NoError(t, err)
	require.Zero(t, files)
}

// A book crossing the per-book threshold is written to its file in one
// spill and continues there; its rows read the same before and after.
//
// Sabotage: skip the existing rows in the spill (load nothing), and rows
// 1..50 are gone after it.
func TestBooks_CrossingTheThresholdSpills(t *testing.T) {
	ti := newBookIndex(t)
	defer SetBookSpillBytesForTest(100 << 10)()
	id := newBook(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 50, 400) // ~23 KiB: stays in memory
	b, _ := ti.OpenBook(id)
	before, err := b.Rows(context.Background(), 1, 50)
	require.NoError(t, err)
	require.False(t, b.Spilled())
	b.Close()
	used := BookMemoryUsed()

	appendUnit(t, ti, id, 2, 51, 300, 400) // past 100 KiB: spills
	b, _ = ti.OpenBook(id)
	defer b.Close()
	require.True(t, b.Spilled())
	require.True(t, exists(ti.bookPath(id)))
	after, err := b.Rows(context.Background(), 1, 50)
	require.NoError(t, err)
	require.Equal(t, before, after, "the spill kept every row")
	units, rows, err := b.Walked(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{2, 350}, []int{units, rows})
	require.Less(t, BookMemoryUsed(), used, "its memory left the budget")
}

// The process budget forces a spill of the LARGEST in-memory book, never a
// refusal: three books under a budget that fits two keep working.
//
// Sabotage: refuse the append over budget (return an error), or spill the
// smallest book, and the test fails.
func TestBooks_GlobalBudgetSpillsTheLargest(t *testing.T) {
	ti := newBookIndex(t)
	base := BookMemoryUsed()
	defer SetBookMemoryBudgetForTest(base + 100<<10)()
	small, large, third := newBook(t, ti, "a"), newBook(t, ti, "b"), newBook(t, ti, "c")
	appendUnit(t, ti, small, 1, 1, 20, 400)  // ~9 KiB
	appendUnit(t, ti, large, 1, 1, 150, 400) // ~69 KiB
	appendUnit(t, ti, third, 1, 1, 60, 400)  // ~28 KiB: over the budget
	inMem, spilled := ti.LiveBooks()
	require.Equal(t, []int{2, 1}, []int{inMem, spilled})
	require.True(t, exists(ti.bookPath(large)), "the largest book was spilled")
	require.False(t, exists(ti.bookPath(small)))
	require.False(t, exists(ti.bookPath(third)))
	require.LessOrEqual(t, BookMemoryUsed(), base+100<<10)
}

// Startup: Open recreates the book directory empty.
//
// Sabotage: drop the RemoveAll in Open, and a file of the last run
// survives.
func TestBookFiles_StartupSweepEmptiesTheDirectory(t *testing.T) {
	main := filepath.Join(t.TempDir(), "k.db")
	db, path, err := Open(main)
	require.NoError(t, err)
	stale := filepath.Join(BooksDirFor(path), "00000000-0000-4000-8000-000000000000.db")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, db.Close())
	db, _, err = Open(main)
	require.NoError(t, err)
	defer db.Close()
	require.False(t, exists(stale), "the previous run's book file is gone")
}

// Expired: a book whose cursor was last used before now - idle TTL - grace
// loses its row and is freed (memory or file), even before the reaper ran;
// a younger one stays.
//
// Sabotage: compare with IdleTTL alone (no grace), and the 17-minute-old
// book is swept.
func TestBookFiles_ExpiredAreFreed(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	ctx := context.Background()
	old, young, oldFile := newBook(t, ti, "a"), newBook(t, ti, "b"), newBook(t, ti, "c")
	appendUnit(t, ti, old, 1, 1, 20, 400)
	spill(t, ti, oldFile)
	setLastUsed(t, ti, old, now.Add(-21*time.Minute))
	setLastUsed(t, ti, oldFile, now.Add(-21*time.Minute))
	setLastUsed(t, ti, young, now.Add(-17*time.Minute))
	used := BookMemoryUsed()
	st, err := ti.SweepBookFiles(ctx, policy(now))
	require.NoError(t, err)
	require.Equal(t, 2, st.Expired)
	_, ok := ti.OpenBook(old)
	require.False(t, ok, "the expired in-memory book is freed")
	require.Less(t, BookMemoryUsed(), used, "and its memory left the budget")
	require.False(t, exists(ti.bookPath(oldFile)), "the expired spilled book's file is gone")
	info, err := ti.GetBook(ctx, old)
	require.NoError(t, err)
	require.Nil(t, info, "its cursor row went with it")
	b, ok := ti.OpenBook(young)
	require.True(t, ok, "inside idle TTL + grace it stays")
	b.Close()
}

// Active: a book a call holds is never dropped, however old its cursor
// looks, and is swept once released.
//
// Sabotage: drop the holders check, and the held book is dropped.
func TestBookFiles_HeldBooksAreKept(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	id := newBook(t, ti, "a")
	spill(t, ti, id)
	setLastUsed(t, ti, id, now.Add(-time.Hour))
	b, ok := ti.OpenBook(id)
	require.True(t, ok)
	st, err := ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Skipped)
	require.True(t, exists(ti.bookPath(id)), "a page in flight keeps its file")
	_, err = b.Results(context.Background())
	require.NoError(t, err, "and can still read it")
	require.NoError(t, b.Close())
	st, err = ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Expired)
	require.False(t, exists(ti.bookPath(id)))
}

// Orphans: a live book whose cursor row is gone is freed; a book file of no
// live book is deleted; sidecars and stray files with no live book go too,
// but a stray file younger than the grace stays.
//
// Sabotage: skip books with no row (no orphan free), or keep every
// non-.db file.
func TestBookFiles_OrphansAndSidecarsAreDeleted(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	live, gone := newBook(t, ti, "a"), newBook(t, ti, "b")
	spill(t, ti, live)
	_, err := ti.db.Exec(`DELETE FROM tool_sessions WHERE id = ?`, gone)
	require.NoError(t, err)
	orphanFile := filepath.Join(ti.booksDir, "11111111-1111-4111-8111-111111111111.db")
	stray := filepath.Join(ti.booksDir, "dead.db-journal")
	liveSide := ti.bookPath(live) + "-journal"
	partial := filepath.Join(ti.booksDir, "tmp-partial")
	fresh := filepath.Join(ti.booksDir, "tmp-fresh")
	for _, p := range []string{orphanFile, stray, liveSide, partial, fresh} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	require.NoError(t, os.Chtimes(partial, now.Add(-time.Hour), now.Add(-time.Hour)))

	st, err := ti.SweepBookFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 2, st.Orphans, "the rowless book and the file of no book")
	_, ok := ti.OpenBook(gone)
	require.False(t, ok)
	require.False(t, exists(orphanFile))
	require.False(t, exists(stray), "a sidecar of no book is deleted")
	require.False(t, exists(partial), "an old stray file is deleted")
	require.True(t, exists(fresh), "a stray file younger than the grace stays")
	require.True(t, exists(liveSide), "a live book's sidecar stays")
	require.True(t, exists(ti.bookPath(live)))
}

// Size cap: over MaxBytes the spilled book idle longest goes first, row
// and file, until the directory fits.
//
// Sabotage: sort newest first, and the most recently used book is evicted.
func TestBookFiles_SizeCapEvictsOldestFirst(t *testing.T) {
	ti := newBookIndex(t)
	now := time.Now()
	ctx := context.Background()
	a, b, c := newBook(t, ti, "a"), newBook(t, ti, "b"), newBook(t, ti, "c")
	for _, id := range []string{a, b, c} {
		spill(t, ti, id)
	}
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

// B1 (review of PR #692): replacement, DeleteBook and CloseBooks never free
// a book a call holds (invariant ad44382e's finalize rule). The book leaves
// service at once (a new OpenBook is refused: its cursor reads as expired),
// its holders keep reading AND appending, and it is freed, memory or file,
// only when the LAST holder releases it.
//
// Sabotage: free it at once in retireLocked (ignore holders), and the
// holder's next read panics on a nil book.
func TestBooks_HeldBookIsFreedOnTheLastRelease(t *testing.T) {
	ctx := context.Background()
	for _, spilled := range []bool{false, true} {
		for _, action := range []string{"replace", "delete", "close"} {
			t.Run(fmt.Sprintf("%s-spilled=%v", action, spilled), func(t *testing.T) {
				ti := newBookIndex(t)
				id := newBook(t, ti, "o")
				appendUnit(t, ti, id, 1, 1, 3, 10)
				if spilled {
					spill(t, ti, id)
				}
				h1, ok := ti.OpenBook(id)
				require.True(t, ok)
				h2, ok := ti.OpenBook(id)
				require.True(t, ok)
				switch action {
				case "replace":
					newBook(t, ti, "o")
				case "delete":
					require.NoError(t, ti.DeleteBook(ctx, id))
				case "close":
					ti.CloseBooks()
				}
				_, ok = ti.OpenBook(id)
				require.False(t, ok, "a retired book is refused to new calls")
				read := func(h *Book, want int) {
					var rows []BookRow
					var err error
					func() {
						defer func() {
							if p := recover(); p != nil {
								require.FailNow(t, "the holder's read PANICKED", "%v", p)
							}
						}()
						rows, err = h.Rows(ctx, 1, 10)
					}()
					require.NoError(t, err)
					require.Len(t, rows, want)
				}
				read(h1, 3)
				require.NoError(t, h1.AppendUnit(ctx, 2, []BookRow{{Seq: 4, Unit: 2, N: 1, Path: "kb/b.md", Written: true, State: "{}"}}, nil, nil, nil),
					"the holder can still walk on")
				h1.Close()
				read(h2, 4)
				if spilled {
					require.True(t, exists(ti.bookPath(id)), "the file stays while a call holds the book")
				}
				h2.Close()
				ti.booksMu.Lock()
				_, still := ti.books[id]
				ti.booksMu.Unlock()
				require.False(t, still, "the last release freed it")
				require.False(t, exists(ti.bookPath(id)), "and deleted its file")
			})
		}
	}
}

// R6 (review of PR #692): the per-book spill counts what the book ALREADY
// holds: two units each under the threshold, together past it, spill.
//
// Sabotage: compare only the new unit's size with the threshold, and a book
// of many small units never spills.
func TestBooks_SpillCountsTheAccumulatedSize(t *testing.T) {
	ti := newBookIndex(t)
	defer SetBookSpillBytesForTest(100 << 10)()
	id := newBook(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 150, 400) // ~70 KiB
	b, _ := ti.OpenBook(id)
	require.False(t, b.Spilled(), "one unit is under the threshold")
	b.Close()
	appendUnit(t, ti, id, 2, 151, 150, 400) // ~70 KiB more: ~140 KiB in all
	b, _ = ti.OpenBook(id)
	defer b.Close()
	require.True(t, b.Spilled(), "the two together pass it")
}

// N9 (review of PR #692): the process budget may spill ANOTHER caller's
// book; when that spill fails, this call's append (already done) still
// succeeds, and the failure is only logged.
//
// Sabotage: return enforceBookBudget's error from AppendUnit, and this
// call fails for the other book's spill.
func TestBooks_ABudgetSpillOfAnotherBookNeverFailsThisCall(t *testing.T) {
	ctx := context.Background()
	other := newBookIndex(t)
	x := newBook(t, other, "x")
	appendUnit(t, other, x, 1, 1, 200, 400) // the largest in-memory book
	require.NoError(t, os.RemoveAll(other.booksDir))
	require.NoError(t, os.WriteFile(other.booksDir, []byte("not a directory"), 0o600)) // x cannot spill

	ti := newBookIndex(t)
	y := newBook(t, ti, "y")
	defer SetBookMemoryBudgetForTest(1)()
	b, ok := ti.OpenBook(y)
	require.True(t, ok)
	defer b.Close()
	err := b.AppendUnit(ctx, 1, []BookRow{{Seq: 1, Unit: 1, N: 1, Path: "kb/a.md", Written: true, State: "{}"}}, nil, nil, nil)
	require.NoError(t, err, "another book's failed spill is not this call's failure")
	rows, err := b.Rows(ctx, 1, 5)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	bx, ok := other.OpenBook(x)
	require.True(t, ok)
	defer bx.Close()
	require.False(t, bx.Spilled(), "the other book's spill did fail")
}
