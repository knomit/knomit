package materialize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

type bookFixture struct {
	ri  *repos.RepoInstance
	svc *store.Service
	b   *repos.Binding
}

func newBookFixture(t testing.TB) *bookFixture {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	})
	return &bookFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
}

func (f *bookFixture) write(t testing.TB, path, title, body string, refs ...string) {
	t.Helper()
	ff := fact.NewFact(path)
	ff.Title, ff.Body, ff.Type = title, body, fact.Observation
	ff.Domain, ff.Entities, ff.Refs = []string{"testing"}, []string{}, refs
	ff.Confidence, ff.Sources = 0.7, 1
	content, err := fact.SerializeFact(ff)
	require.NoError(t, err)
	_, err = f.svc.Facts().WriteFact(context.Background(), "agent/test", path, content, "write "+path, "")
	require.NoError(t, err)
}

func (f *bookFixture) scope(owner string) CursorScope {
	return CursorScope{Sessions: f.svc.ToolSession(), Branch: "agent/test", Pin: f.b.PinID(), ReadSet: "rs", Owner: owner}
}

func (f *bookFixture) request(path string) BookRequest {
	return BookRequest{
		Binding: f.b, Search: BookSearch{Path: path, Sort: SortRelevance}, Facts: DefaultBookFacts,
		Options: DefaultOptions(), Limit: HumanDefaultLimit,
	}
}

// The walk reads every version through the index and never a body: the
// first call's stats show no blob parse and no body read on a corpus the
// index has seen, while every page still carries full bodies.
//
// Sabotage: make readAt skip VersionMetaAt (always parse), and Parsed > 0.
func TestBook_WalkReadsNoBodies(t *testing.T) {
	f := newBookFixture(t)
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/s.md", "kb/u/r1.md")
	page, stats, err := BookFirstPage(context.Background(), f.scope("o"), f.request("kb/u/"))
	require.NoError(t, err)
	require.Equal(t, 2, stats.Units)
	require.Equal(t, 5, stats.Rows)
	require.Positive(t, stats.Walk.Index)
	require.Zero(t, stats.Walk.Parsed, "no version was read from its blob")
	require.Zero(t, stats.Walk.Bodies, "no body was read to compare versions")
	require.NotEmpty(t, page.Facts[0].Body, "pages read bodies lazily")
}

// The row cap refuses a first call before anything is served, names the
// controls to lower, and leaves no cursor behind.
//
// Sabotage: check the cap after appending the unit (>= instead of >), or
// skip the deferred DeleteBook, and the refused build leaves a cursor.
func TestBook_RowCapRefusesAndLeavesNothing(t *testing.T) {
	f := newBookFixture(t)
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/s.md")
	old := maxBookRows
	t.Cleanup(func() { maxBookRows = old })

	maxBookRows = 4 // two units of two rows fit exactly
	_, stats, err := BookFirstPage(context.Background(), f.scope("o"), f.request("kb/u/"))
	require.NoError(t, err)
	require.Equal(t, 4, stats.Rows)

	books, rows, err := f.svc.ToolSession().BookCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{1, 4}, []int{books, rows})

	maxBookRows = 3
	_, _, err = BookFirstPage(context.Background(), f.scope("o2"), f.request("kb/u/"))
	require.ErrorIs(t, err, ErrBookTooLarge)
	require.Contains(t, err.Error(), "lower Facts, Depth or References")
	books, rows, err = f.svc.ToolSession().BookCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{1, 4}, []int{books, rows}, "the refused build left no cursor and no rows")
}

// Over the concurrent-build cap a first call is refused at once (no wait,
// nothing written); a slot that frees is usable again.
//
// Sabotage: make the slot acquisition blocking, and this test times out.
func TestBook_ConcurrentBuildCap(t *testing.T) {
	f := newBookFixture(t)
	f.write(t, "kb/u/r1.md", "R1", "r1 body.")
	release := ReserveBookBuildsForTest(MaxConcurrentBookBuilds)
	_, _, err := BookFirstPage(context.Background(), f.scope("o"), f.request("kb/u/"))
	release()
	require.ErrorIs(t, err, ErrBookBusy)
	_, _, err = BookFirstPage(context.Background(), f.scope("o"), f.request("kb/u/"))
	require.NoError(t, err)
}

// A cursor has one reader at a time: a page served from a stale position
// (another call moved the cursor in between) is refused, never served twice.
//
// Sabotage: drop `AND next_seq = ?` from AdvanceBook, and the stale call is
// served the same rows again.
func TestBook_StalePositionIsRefused(t *testing.T) {
	f := newBookFixture(t)
	f.write(t, "kb/u/r1.md", "R1", "r1 body.")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.")
	sc := f.scope("o")
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := BookFirstPage(context.Background(), sc, req)
	require.NoError(t, err)
	_, env, _, err := openBook(context.Background(), sc, *p1.Cursor)
	require.NoError(t, err)
	// Position is 2 now; a call that read position 1 loses.
	_, err = bookPage(context.Background(), sc, f.b, nil, nil, *p1.Cursor, env, 1, 1, 1, false)
	require.ErrorIs(t, err, ErrCursorBusy)
	p2, err := BookNextPage(context.Background(), sc, f.b, nil, nil, *p1.Cursor, 0)
	require.NoError(t, err)
	require.Equal(t, 2, p2.SeqFrom)
}

// measureEnv gates the measurement runs: they write ~1000 facts with up to
// 499 refs each and walk 250,000 appearances.
const measureEnv = "KNOMIT_BOOK_MEASURE"

// peakHeap samples the Go heap in use every 5 ms until stop is called.
func peakHeap() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return func() uint64 { close(done); wg.Wait(); return peak.Load() }
}

// seedCaps writes the caps corpus: `units` results under kb/u/, each citing
// the same `refs` leaves under kb/z/ (so every unit is 1 + refs sections).
func seedCaps(t testing.TB, f *bookFixture, units, refs int) {
	leaves := make([]string, refs)
	for i := range refs {
		leaves[i] = fmt.Sprintf("kb/z/l%03d.md", i)
		f.write(t, leaves[i], "Leaf "+strconv.Itoa(i), "leaf body "+strconv.Itoa(i)+".")
	}
	for i := range units {
		f.write(t, fmt.Sprintf("kb/u/r%03d.md", i), "Result "+strconv.Itoa(i), "result body.", leaves...)
	}
}

// TestBook_MeasureAtTheCaps measures the first call at the caps: 500 units
// x 499 refs (250,000 appearances, past MaxBookRows, which is lifted for
// the run), its time, heap peak, rows and the session DB's size, and the
// first page and a deep seek. Run with KNOMIT_BOOK_MEASURE=1.
func TestBook_MeasureAtTheCaps(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := 500, 499
	if v := os.Getenv("KNOMIT_BOOK_UNITS"); v != "" {
		units, _ = strconv.Atoi(v)
	}
	f := newBookFixture(t)
	t0 := time.Now()
	seedCaps(t, f, units, refs)
	t.Logf("seed: %d results x %d refs in %s", units, refs, time.Since(t0))

	old := maxBookRows
	maxBookRows = 1 << 30
	t.Cleanup(func() { maxBookRows = old })
	req := f.request("kb/u/")
	req.Options.MaxNodes = MaxNodesLimit
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := peakHeap()
	t1 := time.Now()
	page, stats, err := BookFirstPage(context.Background(), f.scope("o"), req)
	build := time.Since(t1)
	peak := stop()
	require.NoError(t, err)
	size, err := f.svc.ToolSession().BookStorage(context.Background())
	require.NoError(t, err)
	t.Logf("first call: %s, units %d, rows %d, walk %+v, heap before %d MiB, peak %d MiB, session DB %d MiB",
		build, stats.Units, stats.Rows, stats.Walk, before.HeapInuse>>20, peak>>20, size>>20)
	n, total, max := f.svc.ToolSession().BookLockWaits()
	t.Logf("lock waits: %d writes, total %s, max %s", n, total, max)

	t2 := time.Now()
	p, err := BookNextPage(context.Background(), f.scope("o"), f.b, nil, nil, *page.Cursor, units)
	require.NoError(t, err)
	t.Logf("seek to unit %d: %s, %d sections, page seq %d..%d", units, time.Since(t2), len(p.Facts), p.SeqFrom, p.SeqTo)
	t3 := time.Now()
	p, err = BookNextPage(context.Background(), f.scope("o"), f.b, nil, nil, *page.Cursor, 0)
	require.NoError(t, err)
	t.Logf("next page: %s", time.Since(t3))
}

// TestBook_MeasureConcurrentBuilds runs 10 first calls at once on one
// session DB (10 owners), with the build cap lifted to 10 and then at its
// deployed value, and reports time, heap peak and session-DB lock waits.
// KNOMIT_BOOK_UNITS / KNOMIT_BOOK_REFS size each book (default 100 x 199).
func TestBook_MeasureConcurrentBuilds(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := 100, 199
	if v := os.Getenv("KNOMIT_BOOK_UNITS"); v != "" {
		units, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("KNOMIT_BOOK_REFS"); v != "" {
		refs, _ = strconv.Atoi(v)
	}
	f := newBookFixture(t)
	seedCaps(t, f, units, refs)
	old := maxBookRows
	maxBookRows = 1 << 30
	t.Cleanup(func() { maxBookRows = old })

	for _, slots := range []int{10, MaxConcurrentBookBuilds} {
		saved := buildSlots
		buildSlots = make(chan struct{}, slots)
		runtime.GC()
		n0, t0, _ := f.svc.ToolSession().BookLockWaits()
		stop := peakHeap()
		start := time.Now()
		var wg sync.WaitGroup
		var ok, busy atomic.Int32
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := f.request("kb/u/")
				req.Options.MaxNodes = MaxNodesLimit
				_, _, err := BookFirstPage(context.Background(), f.scope(fmt.Sprintf("o%d-%d", slots, i)), req)
				switch {
				case err == nil:
					ok.Add(1)
				case err == ErrBookBusy:
					busy.Add(1)
				default:
					t.Errorf("build %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		peak := stop()
		n1, t1, max := f.svc.ToolSession().BookLockWaits()
		size, _ := f.svc.ToolSession().BookStorage(context.Background())
		t.Logf("10 concurrent builds of %d x %d (cap %d): %s, built %d, refused %d, heap peak %d MiB, session DB %d MiB, lock waits %d writes total %s max-so-far %s",
			units, refs, slots, time.Since(start), ok.Load(), busy.Load(), peak>>20, size>>20, n1-n0, t1-t0, max)
		buildSlots = saved
	}
}
