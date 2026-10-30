package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newFactSetIndex(t *testing.T) *ToolIndex {
	t.Helper()
	db, path, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	ti := NewToolIndex(db, FactSetsDirFor(path))
	t.Cleanup(func() { ti.CloseFactSets(); _ = db.Close() })
	return ti
}

func newFactSet(t *testing.T, ti *ToolIndex, owner string) string {
	t.Helper()
	id, err := ti.CreateFactSet(context.Background(), FactSetToolPrefix+"3", "b", "repo:x", "rs", owner, "{}",
		[]FactSetResult{{Unit: 1, Path: "kb/a.md", Anchor: "f-a"}, {Unit: 2, Path: "kb/b.md", Anchor: "f-b"}}, nil)
	require.NoError(t, err)
	return id
}

// appendUnit appends unit with n rows of size bytes of state each.
func appendUnit(t *testing.T, ti *ToolIndex, id string, unit, from, n, size int) {
	t.Helper()
	b, ok := ti.OpenFactSet(id)
	require.True(t, ok)
	defer b.Close()
	rows := make([]FactSetRow, n)
	for i := range rows {
		rows[i] = FactSetRow{Seq: from + i, Unit: unit, N: i + 1, Path: fmt.Sprintf("kb/z/%d-%d.md", unit, i), Written: true,
			State: fmt.Sprintf("%0*d", size, from+i)}
	}
	require.NoError(t, b.AppendUnit(context.Background(), unit, rows, nil, nil, nil))
}

func spill(t *testing.T, ti *ToolIndex, id string) {
	t.Helper()
	ti.factsetsMu.Lock()
	bs := ti.factsets[id]
	ti.factsetsMu.Unlock()
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

func policy(now time.Time) FactSetSweepPolicy {
	return FactSetSweepPolicy{Now: now, IdleTTL: 15 * time.Minute, Grace: 5 * time.Minute}
}

// A small factset never touches disk: it is created, appended and paged in
// memory, and no file appears.
//
// Sabotage: create the file in CreateFactSet (or spill on every append), and a
// file exists.
func TestFactSets_SmallFactSetNeverCreatesAFile(t *testing.T) {
	ti := newFactSetIndex(t)
	id := newFactSet(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 50, 400)
	b, ok := ti.OpenFactSet(id)
	require.True(t, ok)
	rows, err := b.Rows(context.Background(), 1, 5)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	require.False(t, b.Spilled())
	b.Close()
	require.False(t, exists(ti.factsetPath(id)), "no file for a small factset")
	files, _, err := ti.FactSetFiles()
	require.NoError(t, err)
	require.Zero(t, files)
}

// A factset crossing the per-factset threshold is written to its file in one
// spill and continues there; its rows read the same before and after.
//
// Sabotage: skip the existing rows in the spill (load nothing), and rows
// 1..50 are gone after it.
func TestFactSets_CrossingTheThresholdSpills(t *testing.T) {
	ti := newFactSetIndex(t)
	defer SetFactSetSpillBytesForTest(100 << 10)()
	id := newFactSet(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 50, 400) // ~23 KiB: stays in memory
	b, _ := ti.OpenFactSet(id)
	before, err := b.Rows(context.Background(), 1, 50)
	require.NoError(t, err)
	require.False(t, b.Spilled())
	b.Close()
	used := FactSetMemoryUsed()

	appendUnit(t, ti, id, 2, 51, 300, 400) // past 100 KiB: spills
	b, _ = ti.OpenFactSet(id)
	defer b.Close()
	require.True(t, b.Spilled())
	require.True(t, exists(ti.factsetPath(id)))
	after, err := b.Rows(context.Background(), 1, 50)
	require.NoError(t, err)
	require.Equal(t, before, after, "the spill kept every row")
	units, rows, err := b.Walked(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{2, 350}, []int{units, rows})
	require.Less(t, FactSetMemoryUsed(), used, "its memory left the budget")
}

// The process budget forces a spill of the LARGEST in-memory factset, never a
// refusal: three factsets under a budget that fits two keep working.
//
// Sabotage: refuse the append over budget (return an error), or spill the
// smallest factset, and the test fails.
func TestFactSets_GlobalBudgetSpillsTheLargest(t *testing.T) {
	ti := newFactSetIndex(t)
	base := FactSetMemoryUsed()
	defer SetFactSetMemoryBudgetForTest(base + 100<<10)()
	small, large, third := newFactSet(t, ti, "a"), newFactSet(t, ti, "b"), newFactSet(t, ti, "c")
	appendUnit(t, ti, small, 1, 1, 20, 400)  // ~9 KiB
	appendUnit(t, ti, large, 1, 1, 150, 400) // ~69 KiB
	appendUnit(t, ti, third, 1, 1, 60, 400)  // ~28 KiB: over the budget
	inMem, spilled := ti.LiveFactSets()
	require.Equal(t, []int{2, 1}, []int{inMem, spilled})
	require.True(t, exists(ti.factsetPath(large)), "the largest factset was spilled")
	require.False(t, exists(ti.factsetPath(small)))
	require.False(t, exists(ti.factsetPath(third)))
	require.LessOrEqual(t, FactSetMemoryUsed(), base+100<<10)
}

// Startup: Open recreates the factset directory empty.
//
// Sabotage: drop the RemoveAll in Open, and a file of the last run
// survives.
func TestFactSetFiles_StartupSweepEmptiesTheDirectory(t *testing.T) {
	main := filepath.Join(t.TempDir(), "k.db")
	db, path, err := Open(main)
	require.NoError(t, err)
	stale := filepath.Join(FactSetsDirFor(path), "00000000-0000-4000-8000-000000000000.db")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, db.Close())
	db, _, err = Open(main)
	require.NoError(t, err)
	defer db.Close()
	require.False(t, exists(stale), "the previous run's factset file is gone")
}

// Expired: a factset whose cursor was last used before now - idle TTL - grace
// loses its row and is freed (memory or file), even before the reaper ran;
// a younger one stays.
//
// Sabotage: compare with IdleTTL alone (no grace), and the 17-minute-old
// factset is swept.
func TestFactSetFiles_ExpiredAreFreed(t *testing.T) {
	ti := newFactSetIndex(t)
	now := time.Now()
	ctx := context.Background()
	old, young, oldFile := newFactSet(t, ti, "a"), newFactSet(t, ti, "b"), newFactSet(t, ti, "c")
	appendUnit(t, ti, old, 1, 1, 20, 400)
	spill(t, ti, oldFile)
	setLastUsed(t, ti, old, now.Add(-21*time.Minute))
	setLastUsed(t, ti, oldFile, now.Add(-21*time.Minute))
	setLastUsed(t, ti, young, now.Add(-17*time.Minute))
	used := FactSetMemoryUsed()
	st, err := ti.SweepFactSetFiles(ctx, policy(now))
	require.NoError(t, err)
	require.Equal(t, 2, st.Expired)
	_, ok := ti.OpenFactSet(old)
	require.False(t, ok, "the expired in-memory factset is freed")
	require.Less(t, FactSetMemoryUsed(), used, "and its memory left the budget")
	require.False(t, exists(ti.factsetPath(oldFile)), "the expired spilled factset's file is gone")
	info, err := ti.GetFactSet(ctx, old)
	require.NoError(t, err)
	require.Nil(t, info, "its cursor row went with it")
	b, ok := ti.OpenFactSet(young)
	require.True(t, ok, "inside idle TTL + grace it stays")
	b.Close()
}

// Active: a factset a call holds is never dropped, however old its cursor
// looks, and is swept once released.
//
// Sabotage: drop the holders check, and the held factset is dropped.
func TestFactSetFiles_HeldFactSetsAreKept(t *testing.T) {
	ti := newFactSetIndex(t)
	now := time.Now()
	id := newFactSet(t, ti, "a")
	spill(t, ti, id)
	setLastUsed(t, ti, id, now.Add(-time.Hour))
	b, ok := ti.OpenFactSet(id)
	require.True(t, ok)
	st, err := ti.SweepFactSetFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Skipped)
	require.True(t, exists(ti.factsetPath(id)), "a page in flight keeps its file")
	_, err = b.Results(context.Background())
	require.NoError(t, err, "and can still read it")
	require.NoError(t, b.Close())
	st, err = ti.SweepFactSetFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 1, st.Expired)
	require.False(t, exists(ti.factsetPath(id)))
}

// Orphans: a live factset whose cursor row is gone is freed; a factset file of no
// live factset is deleted; sidecars and stray files with no live factset go too,
// but a stray file younger than the grace stays.
//
// Sabotage: skip factsets with no row (no orphan free), or keep every
// non-.db file.
func TestFactSetFiles_OrphansAndSidecarsAreDeleted(t *testing.T) {
	ti := newFactSetIndex(t)
	now := time.Now()
	live, gone := newFactSet(t, ti, "a"), newFactSet(t, ti, "b")
	spill(t, ti, live)
	_, err := ti.db.Exec(`DELETE FROM tool_sessions WHERE id = ?`, gone)
	require.NoError(t, err)
	orphanFile := filepath.Join(ti.factsetsDir, "11111111-1111-4111-8111-111111111111.db")
	stray := filepath.Join(ti.factsetsDir, "dead.db-journal")
	liveSide := ti.factsetPath(live) + "-journal"
	partial := filepath.Join(ti.factsetsDir, "tmp-partial")
	fresh := filepath.Join(ti.factsetsDir, "tmp-fresh")
	for _, p := range []string{orphanFile, stray, liveSide, partial, fresh} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	require.NoError(t, os.Chtimes(partial, now.Add(-time.Hour), now.Add(-time.Hour)))

	st, err := ti.SweepFactSetFiles(context.Background(), policy(now))
	require.NoError(t, err)
	require.Equal(t, 2, st.Orphans, "the rowless factset and the file of no factset")
	_, ok := ti.OpenFactSet(gone)
	require.False(t, ok)
	require.False(t, exists(orphanFile))
	require.False(t, exists(stray), "a sidecar of no factset is deleted")
	require.False(t, exists(partial), "an old stray file is deleted")
	require.True(t, exists(fresh), "a stray file younger than the grace stays")
	require.True(t, exists(liveSide), "a live factset's sidecar stays")
	require.True(t, exists(ti.factsetPath(live)))
}

// Size cap: over MaxBytes the spilled factset idle longest goes first, row
// and file, until the directory fits.
//
// Sabotage: sort newest first, and the most recently used factset is evicted.
func TestFactSetFiles_SizeCapEvictsOldestFirst(t *testing.T) {
	ti := newFactSetIndex(t)
	now := time.Now()
	ctx := context.Background()
	a, b, c := newFactSet(t, ti, "a"), newFactSet(t, ti, "b"), newFactSet(t, ti, "c")
	for _, id := range []string{a, b, c} {
		spill(t, ti, id)
	}
	setLastUsed(t, ti, a, now.Add(-3*time.Minute))
	setLastUsed(t, ti, b, now.Add(-1*time.Minute))
	setLastUsed(t, ti, c, now.Add(-2*time.Minute))
	fi, err := os.Stat(ti.factsetPath(a))
	require.NoError(t, err)
	p := policy(now)
	p.MaxBytes = 2 * fi.Size()
	st, err := ti.SweepFactSetFiles(ctx, p)
	require.NoError(t, err)
	require.Equal(t, 1, st.Evicted)
	require.False(t, exists(ti.factsetPath(a)), "the factset idle longest is evicted")
	info, err := ti.GetFactSet(ctx, a)
	require.NoError(t, err)
	require.Nil(t, info, "its cursor is gone too: it reads as expired")
	require.True(t, exists(ti.factsetPath(b)))
	require.True(t, exists(ti.factsetPath(c)))
}

// B1 (review of PR #692): replacement, DeleteFactSet and CloseFactSets never free
// a factset a call holds (invariant ad44382e's finalize rule). The factset leaves
// service at once (a new OpenFactSet is refused: its cursor reads as expired),
// its holders keep reading AND appending, and it is freed, memory or file,
// only when the LAST holder releases it.
//
// Sabotage: free it at once in retireLocked (ignore holders), and the
// holder's next read panics on a nil factset.
func TestFactSets_HeldFactSetIsFreedOnTheLastRelease(t *testing.T) {
	ctx := context.Background()
	for _, spilled := range []bool{false, true} {
		for _, action := range []string{"replace", "delete", "close"} {
			t.Run(fmt.Sprintf("%s-spilled=%v", action, spilled), func(t *testing.T) {
				ti := newFactSetIndex(t)
				id := newFactSet(t, ti, "o")
				appendUnit(t, ti, id, 1, 1, 3, 10)
				if spilled {
					spill(t, ti, id)
				}
				h1, ok := ti.OpenFactSet(id)
				require.True(t, ok)
				h2, ok := ti.OpenFactSet(id)
				require.True(t, ok)
				switch action {
				case "replace":
					newFactSet(t, ti, "o")
				case "delete":
					require.NoError(t, ti.DeleteFactSet(ctx, id))
				case "close":
					ti.CloseFactSets()
				}
				_, ok = ti.OpenFactSet(id)
				require.False(t, ok, "a retired factset is refused to new calls")
				read := func(h *FactSet, want int) {
					var rows []FactSetRow
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
				require.NoError(t, h1.AppendUnit(ctx, 2, []FactSetRow{{Seq: 4, Unit: 2, N: 1, Path: "kb/b.md", Written: true, State: "{}"}}, nil, nil, nil),
					"the holder can still walk on")
				h1.Close()
				read(h2, 4)
				if spilled {
					require.True(t, exists(ti.factsetPath(id)), "the file stays while a call holds the factset")
				}
				h2.Close()
				ti.factsetsMu.Lock()
				_, still := ti.factsets[id]
				ti.factsetsMu.Unlock()
				require.False(t, still, "the last release freed it")
				require.False(t, exists(ti.factsetPath(id)), "and deleted its file")
			})
		}
	}
}

// The batch lookups (round 4) answer exactly as the per-item ones, in
// memory and after a spill: AnchorsTaken as AnchorTaken (a result's anchor,
// a reserved anchor, a unit's anchor, a free one), ReservedOf as Reserved.
//
// Sabotage: let the file form of a batch lookup read the wrong table or
// column, and the spilled answers differ.
func TestFactSets_BatchLookupsAreThePerItemOnes(t *testing.T) {
	ctx := context.Background()
	ti := newFactSetIndex(t)
	results := []FactSetResult{{Unit: 1, Path: "kb/a.md", Anchor: "f-a"}, {Unit: 2, Path: "kb/b.md", Anchor: "f-b"}}
	reserved := []FactSetReserve{{Path: "kb/z/x.md", Unit: 1, Number: "1.1", Anchor: "f-x", Commit: "c", Blob: "b"}}
	id, err := ti.CreateFactSet(ctx, FactSetToolPrefix+"3", "b", "repo:x", "rs", "o", "{}", results, reserved)
	require.NoError(t, err)
	fs, ok := ti.OpenFactSet(id)
	require.True(t, ok)
	require.NoError(t, fs.AppendUnit(ctx, 1, []FactSetRow{{Seq: 1, Unit: 1, N: 1, Path: "kb/a.md", Written: true, State: "{}"}},
		nil, []string{"f-y"}, nil))
	fs.Close()
	anchors := []string{"f-a", "f-b", "f-x", "f-y", "f-free"}
	paths := []string{"kb/z/x.md", "kb/a.md", "kb/none.md"}
	check := func(label string) {
		fs, ok := ti.OpenFactSet(id)
		require.True(t, ok)
		defer fs.Close()
		taken, err := fs.AnchorsTaken(ctx, anchors)
		require.NoError(t, err)
		for _, a := range anchors {
			one, err := fs.AnchorTaken(ctx, a)
			require.NoError(t, err)
			require.Equal(t, one, taken[a], "%s: anchor %s", label, a)
		}
		res, err := fs.ReservedOf(ctx, paths)
		require.NoError(t, err)
		for _, p := range paths {
			one, ok, err := fs.Reserved(ctx, p)
			require.NoError(t, err)
			got, gotOK := res[p]
			require.Equal(t, ok, gotOK, "%s: %s reserved", label, p)
			require.Equal(t, one, got, "%s: %s", label, p)
		}
		require.Len(t, taken, 4, "%s: every anchor but the free one is taken", label)
		require.Len(t, res, 1)
	}
	check("memory")
	spill(t, ti, id)
	check("file")
}

// The reserved set (two-level reservation) is part of a factset's structure:
// it counts toward the factset's accounted size (the spill threshold and the
// process budget), and a spill carries it to the file.
//
// Sabotage: leave the reservations out of newMemFactSet's size, or out of the
// spilled file, and a check fails.
func TestFactSets_ReservedSetIsCountedAndSpilled(t *testing.T) {
	ti := newFactSetIndex(t)
	ctx := context.Background()
	results := []FactSetResult{{Unit: 1, Path: "kb/a.md", Anchor: "f-a"}}
	before := FactSetMemoryUsed()
	plain, err := ti.CreateFactSet(ctx, FactSetToolPrefix+"3", "b", "repo:x", "rs", "plain", "{}", results, nil)
	require.NoError(t, err)
	plainSize := FactSetMemoryUsed() - before
	var reserved []FactSetReserve
	for i := range 100 {
		reserved = append(reserved, FactSetReserve{Path: fmt.Sprintf("kb/z/r%03d.md", i), Unit: 1, Number: fmt.Sprintf("1.%d", i+1),
			Anchor: fmt.Sprintf("f-r%03d", i), Commit: strings.Repeat("c", 40), Blob: strings.Repeat("b", 40)})
	}
	before = FactSetMemoryUsed()
	id, err := ti.CreateFactSet(ctx, FactSetToolPrefix+"3", "b", "repo:x", "rs", "reserving", "{}", results, reserved)
	require.NoError(t, err)
	require.GreaterOrEqual(t, FactSetMemoryUsed()-before-plainSize, int64(100*(len("kb/z/r000.md")+40+40)),
		"the reserved set is accounted")
	_ = plain

	spill(t, ti, id)
	b, ok := ti.OpenFactSet(id)
	require.True(t, ok)
	defer b.Close()
	require.True(t, b.Spilled())
	r, ok, err := b.Reserved(ctx, "kb/z/r042.md")
	require.NoError(t, err)
	require.True(t, ok, "the spill carried the reservations")
	require.Equal(t, FactSetReserve{Path: "kb/z/r042.md", Unit: 1, Number: "1.43", Anchor: "f-r042", Commit: strings.Repeat("c", 40), Blob: strings.Repeat("b", 40)}, r)
	taken, err := b.AnchorTaken(ctx, "f-r042")
	require.NoError(t, err)
	require.True(t, taken, "a reserved anchor is taken")
}

// R6 (review of PR #692): the per-factset spill counts what the factset ALREADY
// holds: two units each under the threshold, together past it, spill.
//
// Sabotage: compare only the new unit's size with the threshold, and a factset
// of many small units never spills.
func TestFactSets_SpillCountsTheAccumulatedSize(t *testing.T) {
	ti := newFactSetIndex(t)
	defer SetFactSetSpillBytesForTest(100 << 10)()
	id := newFactSet(t, ti, "o")
	appendUnit(t, ti, id, 1, 1, 150, 400) // ~70 KiB
	b, _ := ti.OpenFactSet(id)
	require.False(t, b.Spilled(), "one unit is under the threshold")
	b.Close()
	appendUnit(t, ti, id, 2, 151, 150, 400) // ~70 KiB more: ~140 KiB in all
	b, _ = ti.OpenFactSet(id)
	defer b.Close()
	require.True(t, b.Spilled(), "the two together pass it")
}

// N9 (review of PR #692): the process budget may spill ANOTHER caller's
// factset; when that spill fails, this call's append (already done) still
// succeeds, and the failure is only logged.
//
// Sabotage: return enforceFactSetBudget's error from AppendUnit, and this
// call fails for the other factset's spill.
func TestFactSets_ABudgetSpillOfAnotherFactSetNeverFailsThisCall(t *testing.T) {
	ctx := context.Background()
	other := newFactSetIndex(t)
	x := newFactSet(t, other, "x")
	appendUnit(t, other, x, 1, 1, 200, 400) // the largest in-memory factset
	require.NoError(t, os.RemoveAll(other.factsetsDir))
	require.NoError(t, os.WriteFile(other.factsetsDir, []byte("not a directory"), 0o600)) // x cannot spill

	ti := newFactSetIndex(t)
	y := newFactSet(t, ti, "y")
	defer SetFactSetMemoryBudgetForTest(1)()
	b, ok := ti.OpenFactSet(y)
	require.True(t, ok)
	defer b.Close()
	err := b.AppendUnit(ctx, 1, []FactSetRow{{Seq: 1, Unit: 1, N: 1, Path: "kb/a.md", Written: true, State: "{}"}}, nil, nil, nil)
	require.NoError(t, err, "another factset's failed spill is not this call's failure")
	rows, err := b.Rows(ctx, 1, 5)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	bx, ok := other.OpenFactSet(x)
	require.True(t, ok)
	defer bx.Close()
	require.False(t, bx.Spilled(), "the other factset's spill did fail")
}
