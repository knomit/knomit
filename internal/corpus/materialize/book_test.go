package materialize

import (
	"context"
	"errors"
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

// fixtureTwo writes r1 -> s and then r2 -> s, r1: the search lists r2 first
// (newest commit, or by path on a tie... r2 > r1, so the newest decides; a
// tie keeps r1 first). Tests below read the unit order from the book.
func fixtureTwo(t *testing.T, f *bookFixture) {
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/s.md", "kb/u/r1.md")
}

// The walk reads every version through the index and never a body: the
// stats show no blob parse and no body read on a corpus the index has seen,
// while every page still carries full bodies.
//
// Sabotage: make readAt skip VersionMetaAt (always parse), and Parsed > 0.
func TestBook_WalkReadsNoBodies(t *testing.T) {
	f := newBookFixture(t)
	fixtureTwo(t, f)
	req := f.request("kb/u/")
	req.Limit = 5
	page, stats, err := BookFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 2, stats.UnitsWalked)
	require.Equal(t, 5, stats.Rows, "unit of r2: r2, s, r1 (link); unit of r1: r1, s (link)")
	require.Positive(t, stats.Walk.Index)
	require.Zero(t, stats.Walk.Parsed, "no version was read from its blob")
	require.Zero(t, stats.Walk.Bodies, "no body was read to compare versions")
	require.NotEmpty(t, page.Facts[0].Body, "pages read bodies lazily")
}

// The row cap: the units walked still page (a short page), the call that
// needs the next unit is refused naming the controls to lower, and a first
// call that cannot serve page 1 leaves no cursor and no file.
//
// Sabotage: check the cap with >= (unit 1's two rows already trip a cap of
// 2), or drop the first call's DeleteBook (a refused first call leaves a
// cursor).
func TestBook_RowCap(t *testing.T) {
	f := newBookFixture(t)
	fixtureTwo(t, f)
	old := maxBookRows
	t.Cleanup(func() { maxBookRows = old })
	ctx := context.Background()

	one := f.request("kb/u/")
	one.Limit = 1
	probe, _, err := BookFirstPage(ctx, f.scope("probe"), one)
	require.NoError(t, err)
	maxBookRows = probe.Sections // unit 1 fits exactly, unit 2 does not
	p1, _, err := BookFirstPage(ctx, f.scope("o"), f.request("kb/u/"))
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked)
	require.True(t, p1.HasMore)
	var nerr error
	for c := *p1.Cursor; nerr == nil; {
		_, nerr = BookNextPage(ctx, f.scope("o"), f.b, nil, nil, c, 0)
	}
	require.ErrorIs(t, nerr, ErrBookTooLarge)
	require.Contains(t, nerr.Error(), "lower Facts, Depth or References")

	maxBookRows = 1 // not even unit 1
	_, _, err = BookFirstPage(ctx, f.scope("o2"), f.request("kb/u/"))
	require.ErrorIs(t, err, ErrBookTooLarge)
	books, err := f.svc.ToolSession().BookCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, books, "the refused first call left no cursor (probe and o remain)")
	files, _, err := f.svc.ToolSession().BookFiles()
	require.NoError(t, err)
	require.Equal(t, 2, files, "and no file")
}

// Over the concurrent-walk cap a call that must walk is refused at once (no
// wait, nothing written); a slot that frees is usable again; a page served
// from units already walked takes no slot.
//
// Sabotage: make the slot acquisition blocking, and this test times out.
func TestBook_ConcurrentBuildCap(t *testing.T) {
	f := newBookFixture(t)
	fixtureTwo(t, f)
	ctx := context.Background()
	release := ReserveBookBuildsForTest(MaxConcurrentBookBuilds)
	_, _, err := BookFirstPage(ctx, f.scope("o"), f.request("kb/u/"))
	release()
	require.ErrorIs(t, err, ErrBookBusy)
	req := f.request("kb/u/")
	req.Limit = 1
	p, _, err := BookFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	release = ReserveBookBuildsForTest(MaxConcurrentBookBuilds)
	_, err = BookNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
	require.NoError(t, err, "row 2 is in unit 1, already walked: no slot needed")
	_, err = BookNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 2)
	release()
	require.ErrorIs(t, err, ErrBookBusy, "a seek that must walk unit 2 needs a slot")
}

// A cursor has one reader at a time: a page served from a stale position
// (another call moved the cursor in between) is refused, never served twice.
//
// Sabotage: drop `AND next_seq = ?` from AdvanceBook, and the stale call is
// served the same rows again.
func TestBook_StalePositionIsRefused(t *testing.T) {
	f := newBookFixture(t)
	fixtureTwo(t, f)
	ctx := context.Background()
	sc := f.scope("o")
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := BookFirstPage(ctx, sc, req)
	require.NoError(t, err)
	bk, _, err := openBook(ctx, sc, f.b, nil, nil, *p1.Cursor)
	require.NoError(t, err)
	defer bk.close()
	// Position is 2 now; a call that read position 1 loses.
	_, err = bk.page(1, 1, false)
	require.ErrorIs(t, err, ErrCursorBusy)
	p2, err := BookNextPage(ctx, sc, f.b, nil, nil, *p1.Cursor, 0)
	require.NoError(t, err)
	require.Equal(t, 2, p2.SeqFrom)
}

// A seek to a unit not walked yet walks the units before it first (the
// visited set of unit N depends on them), then serves unit N.
//
// Sabotage: walk only unit N on a seek, and its link to the earlier unit's
// s is written instead of linked.
func TestBook_SeekWalksTheUnitsBefore(t *testing.T) {
	f := newBookFixture(t)
	fixtureTwo(t, f)
	f.write(t, "kb/u/r3.md", "R3", "r3 body.", "kb/z/s.md")
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := BookFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked)
	p, stats, err := BookNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p1.Cursor, 3)
	require.NoError(t, err)
	require.Equal(t, 2, stats.UnitsWalked, "the seek walked units 2 and 3 (unit 1 was walked)")
	require.Equal(t, 3, p.UnitFrom)
	require.Equal(t, 3, p.UnitsWalked)
	require.False(t, p.Facts[0].Written == false, "unit 3's root is written")
	if len(p.Facts) > 0 {
		for _, n := range p.Facts {
			if n.Path == "kb/z/s.md" {
				require.NotNil(t, n.Link, "s was written in an earlier unit: a link here")
			}
		}
	}
}

// measureEnv gates the measurement runs: they write ~1000 facts with up to
// 499 refs each and walk up to 250,000 sections.
const measureEnv = "KNOMIT_BOOK_MEASURE"

// peakSampler samples the Go heap in use and the open book files every
// 5 ms until stop is called.
func peakSampler(ti interface{ OpenBookFiles() int }) (stop func() (heap uint64, files int)) {
	var peak atomic.Uint64
	var files atomic.Int64
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
			if n := int64(ti.OpenBookFiles()); n > files.Load() {
				files.Store(n)
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return func() (uint64, int) { close(done); wg.Wait(); return peak.Load(), int(files.Load()) }
}

// seedCaps writes the caps corpus: `units` results under kb/u/, each citing
// the same `refs` leaves under kb/z/.
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

// distinct is the caps corpus where no two results share a cited fact:
// `units` results, each citing `refs` leaves of its own. It is the
// worst case for the walk (no version is read twice).
func seedDistinct(t testing.TB, f *bookFixture, units, refs int) {
	for i := range units {
		leaves := make([]string, refs)
		for j := range refs {
			leaves[j] = fmt.Sprintf("kb/z/%03d/l%03d.md", i, j)
			f.write(t, leaves[j], fmt.Sprintf("Leaf %d.%d", i, j), "leaf body.")
		}
		f.write(t, fmt.Sprintf("kb/u/r%03d.md", i), "Result "+strconv.Itoa(i), "result body.", leaves...)
	}
}

// TestBook_MeasureLargest measures the largest book: KNOMIT_BOOK_UNITS
// results x KNOMIT_BOOK_REFS refs (default 500 x 499 = 250,000 walk
// sections; MaxBookRows is lifted for the run; KNOMIT_BOOK_DISTINCT=1 gives
// every result its own leaves). It reports page 1, the far seek to the last
// unit (which walks every unit before it), the heap peak, the book file's
// size, the open book files and the shared session DB's lock waits.
func TestBook_MeasureLargest(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := envInt("KNOMIT_BOOK_UNITS", 500), envInt("KNOMIT_BOOK_REFS", 499)
	f := newBookFixture(t)
	t0 := time.Now()
	if os.Getenv("KNOMIT_BOOK_DISTINCT") != "" {
		seedDistinct(t, f, units, refs)
	} else {
		seedCaps(t, f, units, refs)
	}
	t.Logf("seed: %d results x %d refs in %s", units, refs, time.Since(t0))
	old := maxBookRows
	maxBookRows = 1 << 30
	t.Cleanup(func() { maxBookRows = old })
	req := f.request("kb/u/")
	req.Options.MaxNodes = MaxNodesLimit
	ctx := context.Background()
	ti := f.svc.ToolSession()

	runtime.GC()
	stop := peakSampler(ti)
	t1 := time.Now()
	page, stats, err := BookFirstPage(ctx, f.scope("o"), req)
	first := time.Since(t1)
	require.NoError(t, err)
	t2 := time.Now()
	p, sstats, err := BookNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *page.Cursor, units)
	seek := time.Since(t2)
	heap, files := stop()
	require.NoError(t, err)
	nfiles, bytes, _ := ti.BookFiles()
	n, total, max := ti.BookLockWaits()
	t.Logf("page 1: %s (walked %d units, %d sections); far seek to unit %d: %s (walked %d units, %d sections, walk %+v); heap peak %d MiB; book files %d, %d MiB; open book files peak %d; session-DB lock waits %d writes, total %s, max %s",
		first, stats.UnitsWalked, stats.Rows, units, seek, sstats.UnitsWalked, p.Sections, sstats.Walk, heap>>20, nfiles, bytes>>20, files, n, total, max)
	t3 := time.Now()
	_, err = BookNextPage(ctx, f.scope("o"), f.b, nil, nil, *page.Cursor, 1)
	require.NoError(t, err)
	t.Logf("seek back to unit 1 (walked): %s", time.Since(t3))
}

// TestBook_MeasureConcurrentBooks runs 10 books at once on one session
// store (10 owners), each a first call then a far seek to its last unit,
// with the walk cap lifted to 10 and then at its deployed value.
func TestBook_MeasureConcurrentBooks(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := envInt("KNOMIT_BOOK_UNITS", 500), envInt("KNOMIT_BOOK_REFS", 199)
	f := newBookFixture(t)
	seedCaps(t, f, units, refs)
	old := maxBookRows
	maxBookRows = 1 << 30
	t.Cleanup(func() { maxBookRows = old })
	ti := f.svc.ToolSession()

	for _, slots := range []int{10, MaxConcurrentBookBuilds} {
		saved := buildSlots
		buildSlots = make(chan struct{}, slots)
		runtime.GC()
		n0, t0, _ := ti.BookLockWaits()
		stop := peakSampler(ti)
		start := time.Now()
		var wg sync.WaitGroup
		var ok, busy atomic.Int32
		var maxSeek atomic.Int64
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := context.Background()
				req := f.request("kb/u/")
				req.Options.MaxNodes = MaxNodesLimit
				owner := fmt.Sprintf("o%d-%d", slots, i)
				p, _, err := BookFirstPage(ctx, f.scope(owner), req)
				if err == nil {
					s := time.Now()
					_, err = BookNextPage(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, units)
					if d := int64(time.Since(s)); err == nil && d > maxSeek.Load() {
						maxSeek.Store(d)
					}
				}
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrBookBusy):
					busy.Add(1)
				default:
					t.Errorf("book %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		heap, files := stop()
		n1, t1, max := ti.BookLockWaits()
		nfiles, bytes, _ := ti.BookFiles()
		t.Logf("10 concurrent books of %d x %d (walk cap %d): %s, completed %d, refused %d, slowest far seek %s, heap peak %d MiB, book files %d (%d MiB, %d MiB each), open book files peak %d, session-DB lock waits %d writes total %s max-so-far %s",
			units, refs, slots, time.Since(start), ok.Load(), busy.Load(), time.Duration(maxSeek.Load()), heap>>20, nfiles, bytes>>20, (bytes/int64(max1(nfiles)))>>20, files, n1-n0, t1-t0, max)
		buildSlots = saved
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
