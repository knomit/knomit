package materialize

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/sessions"
)

type factsetFixture struct {
	ri  *repos.RepoInstance
	svc *store.Service
	b   *repos.Binding
}

func newFactSetFixture(t testing.TB) *factsetFixture {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	})
	return &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
}

func (f *factsetFixture) write(t testing.TB, path, title, body string, refs ...string) {
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

func (f *factsetFixture) scope(owner string) CursorScope {
	return CursorScope{Sessions: f.svc.ToolSession(), Branch: "agent/test", Pin: f.b.PinID(), ReadSet: "rs", Owner: owner}
}

func (f *factsetFixture) request(path string) FactSetRequest {
	return FactSetRequest{
		Binding: f.b, Search: FactSetSearch{Path: path, Sort: SortRelevance}, Facts: DefaultFactSetFacts,
		Options: DefaultOptions(), Limit: HumanDefaultLimit,
	}
}

// fixtureTwo writes r1 -> s and then r2 -> s, r1: the search lists r2 first
// (newest commit, or by path on a tie... r2 > r1, so the newest decides; a
// tie keeps r1 first). Tests below read the unit order from the factset.
func fixtureTwo(t *testing.T, f *factsetFixture) {
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/s.md", "kb/u/r1.md")
}

// The walk reads every version through the index and never a body: the
// stats show no blob parse and no body read on a corpus the index has seen,
// while every page still carries full bodies.
//
// Sabotage: make readAt skip VersionMetaAt (always parse), and Parsed > 0.
func TestFactSet_WalkReadsNoBodies(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	req := f.request("kb/u/")
	req.Limit = 5
	page, stats, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
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
// 2), or drop the first call's DeleteFactSet (a refused first call leaves a
// cursor).
func TestFactSet_RowCap(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	old := maxFactSetRows
	t.Cleanup(func() { maxFactSetRows = old })
	ctx := context.Background()

	one := f.request("kb/u/")
	one.Limit = 1
	probe, _, err := FactSetFirstPage(ctx, f.scope("probe"), one)
	require.NoError(t, err)
	maxFactSetRows = probe.Sections // unit 1 fits exactly, unit 2 does not
	p1, _, err := FactSetFirstPage(ctx, f.scope("o"), f.request("kb/u/"))
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked)
	require.True(t, p1.HasMore)
	var nerr error
	for c := *p1.Cursor; nerr == nil; {
		_, nerr = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, c, 0)
	}
	require.ErrorIs(t, nerr, ErrFactSetTooLarge)
	require.Contains(t, nerr.Error(), "lower Facts, Depth or References")

	maxFactSetRows = 1 // not even unit 1
	_, _, err = FactSetFirstPage(ctx, f.scope("o2"), f.request("kb/u/"))
	require.ErrorIs(t, err, ErrFactSetTooLarge)
	factsets, err := f.svc.ToolSession().FactSetCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, factsets, "the refused first call left no cursor (probe and o remain)")
	inMem, spilled := f.svc.ToolSession().LiveFactSets()
	require.Equal(t, 2, inMem+spilled, "and no live factset")
}

// A factset that spills mid-read pages exactly as one that never spills: the
// same bytes page for page (cursor ids aside), before and after the spill.
//
// Sabotage: drop the rows from the spill's load, and the pages after it are
// missing or wrong.
func TestFactSet_PagesAreTheSameAcrossASpill(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	f.write(t, "kb/u/r3.md", "R3", "r3 body.", "kb/z/s.md", "kb/u/r2.md")
	ctx := context.Background()
	read := func(owner string, spillAfter int) ([]string, bool) {
		req := f.request("kb/u/")
		req.Limit = 1
		p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
		require.NoError(t, err)
		c := *p.Cursor
		var out []string
		for i := 0; ; i++ {
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			out = append(out, strings.ReplaceAll(string(raw), c, "CURSOR"))
			if i == spillAfter {
				defer sessions.SetFactSetSpillBytesForTest(1)() // the next unit's append spills
			}
			if !p.HasMore {
				break
			}
			p, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, c, 0)
			require.NoError(t, err)
		}
		bk, ok := f.svc.ToolSession().OpenFactSet(c)
		require.True(t, ok)
		defer bk.Close()
		return out, bk.Spilled()
	}
	memory, spilledA := read("mem", 1000)
	require.False(t, spilledA, "a small factset stays in memory")
	spilledPages, spilledB := read("spill", 0)
	require.True(t, spilledB, "this one spilled after page 1")
	require.Equal(t, memory, spilledPages)
}

// Over the concurrent-walk cap a call that must walk is refused at once (no
// wait, nothing written); a slot that frees is usable again; a page served
// from units already walked takes no slot.
//
// Sabotage: make the slot acquisition blocking, and this test times out.
func TestFactSet_ConcurrentBuildCap(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	ctx := context.Background()
	release := ReserveFactSetBuildsForTest(MaxConcurrentFactSetBuilds)
	_, _, err := FactSetFirstPage(ctx, f.scope("o"), f.request("kb/u/"))
	release()
	require.ErrorIs(t, err, ErrFactSetBusy)
	req := f.request("kb/u/")
	req.Limit = 1
	p, _, err := FactSetFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	release = ReserveFactSetBuildsForTest(MaxConcurrentFactSetBuilds)
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
	require.NoError(t, err, "row 2 is in unit 1, already walked: no slot needed")
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 2)
	release()
	require.ErrorIs(t, err, ErrFactSetBusy, "a seek that must walk unit 2 needs a slot")
}

// A cursor has one reader at a time: a page served from a stale position
// (another call moved the cursor in between) is refused, never served twice.
//
// Sabotage: drop `AND next_seq = ?` from AdvanceFactSet, and the stale call is
// served the same rows again.
func TestFactSet_StalePositionIsRefused(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	ctx := context.Background()
	sc := f.scope("o")
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := FactSetFirstPage(ctx, sc, req)
	require.NoError(t, err)
	bk, _, err := openFactSet(ctx, sc, f.b, nil, nil, *p1.Cursor)
	require.NoError(t, err)
	defer bk.close()
	// Position is 2 now; a call that read position 1 loses.
	_, err = bk.page(1, 1, false)
	require.ErrorIs(t, err, ErrCursorInUse)
	p2, err := FactSetNextPage(ctx, sc, f.b, nil, nil, *p1.Cursor, 0)
	require.NoError(t, err)
	require.Equal(t, 2, p2.SeqFrom)
}

// A seek to a unit not walked yet walks the units before it first (the
// visited set of unit N depends on them), then serves unit N.
//
// Sabotage: walk only unit N on a seek, and its link to the earlier unit's
// s is written instead of linked.
func TestFactSet_SeekWalksTheUnitsBefore(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	f.write(t, "kb/u/r3.md", "R3", "r3 body.", "kb/z/s.md")
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := FactSetFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked)
	p, stats, err := FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p1.Cursor, 3)
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
const measureEnv = "KNOMIT_FACTSET_MEASURE"

// peakSampler samples the Go heap in use and the open factset files every
// 5 ms until stop is called.
func peakSampler(ti interface{ OpenFactSetFiles() int }) (stop func() (heap uint64, files int)) {
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
			if n := int64(ti.OpenFactSetFiles()); n > files.Load() {
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
func seedCaps(t testing.TB, f *factsetFixture, units, refs int) {
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
func seedDistinct(t testing.TB, f *factsetFixture, units, refs int) {
	for i := range units {
		leaves := make([]string, refs)
		for j := range refs {
			leaves[j] = fmt.Sprintf("kb/z/%03d/l%03d.md", i, j)
			f.write(t, leaves[j], fmt.Sprintf("Leaf %d.%d", i, j), "leaf body.")
		}
		f.write(t, fmt.Sprintf("kb/u/r%03d.md", i), "Result "+strconv.Itoa(i), "result body.", leaves...)
	}
}

// TestFactSet_MeasureLargest measures the largest factset: KNOMIT_FACTSET_UNITS
// results x KNOMIT_FACTSET_REFS refs (default 500 x 499 = 250,000 walk
// sections; MaxFactSetRows is lifted for the run; KNOMIT_FACTSET_DISTINCT=1 gives
// every result its own leaves). It reports page 1, the far seek to the last
// unit (which walks every unit before it), the heap peak, the factset file's
// size, the open factset files and the shared session DB's lock waits.
func TestFactSet_MeasureLargest(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := envInt("KNOMIT_FACTSET_UNITS", 500), envInt("KNOMIT_FACTSET_REFS", 499)
	f := newFactSetFixture(t)
	t0 := time.Now()
	if os.Getenv("KNOMIT_FACTSET_DISTINCT") != "" {
		seedDistinct(t, f, units, refs)
	} else {
		seedCaps(t, f, units, refs)
	}
	t.Logf("seed: %d results x %d refs in %s", units, refs, time.Since(t0))
	old := maxFactSetRows
	maxFactSetRows = 1 << 30
	t.Cleanup(func() { maxFactSetRows = old })
	req := f.request("kb/u/")
	req.Options.MaxNodes = MaxNodesLimit
	ctx := context.Background()
	ti := f.svc.ToolSession()

	runtime.GC()
	stop := peakSampler(ti)
	t1 := time.Now()
	page, stats, err := FactSetFirstPage(ctx, f.scope("o"), req)
	first := time.Since(t1)
	require.NoError(t, err)
	t2 := time.Now()
	p, sstats, err := FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *page.Cursor, units)
	seek := time.Since(t2)
	heap, files := stop()
	require.NoError(t, err)
	nfiles, bytes, _ := ti.FactSetFiles()
	n, total, max := ti.FactSetLockWaits()
	inMem, spilled := ti.LiveFactSets()
	t.Logf("page 1: %s (walked %d units, %d sections); far seek to unit %d: %s (walked %d units, %d sections, walk %+v); heap peak %d MiB; factsets in memory %d, spilled %d (factset memory now %d KiB); factset files %d, %d MiB; open factset files peak %d; session-DB lock waits %d writes, total %s, max %s",
		first, stats.UnitsWalked, stats.Rows, units, seek, sstats.UnitsWalked, p.Sections, sstats.Walk, heap>>20, inMem, spilled, sessions.FactSetMemoryUsed()>>10, nfiles, bytes>>20, files, n, total, max)
	t3 := time.Now()
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *page.Cursor, 1)
	require.NoError(t, err)
	t.Logf("seek back to unit 1 (walked): %s", time.Since(t3))
}

// TestFactSet_MeasureConcurrentFactSets runs 10 factsets at once on one session
// store (10 owners), each a first call then a far seek to its last unit,
// with the walk cap lifted to 10 and then at its deployed value.
func TestFactSet_MeasureConcurrentFactSets(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skip("set " + measureEnv + "=1 to measure")
	}
	units, refs := envInt("KNOMIT_FACTSET_UNITS", 500), envInt("KNOMIT_FACTSET_REFS", 199)
	f := newFactSetFixture(t)
	seedCaps(t, f, units, refs)
	old := maxFactSetRows
	maxFactSetRows = 1 << 30
	t.Cleanup(func() { maxFactSetRows = old })
	ti := f.svc.ToolSession()

	for _, slots := range []int{10, MaxConcurrentFactSetBuilds} {
		saved := buildSlots
		buildSlots = make(chan struct{}, slots)
		runtime.GC()
		n0, t0, _ := ti.FactSetLockWaits()
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
				p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
				if err == nil {
					s := time.Now()
					_, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, units)
					if d := int64(time.Since(s)); err == nil && d > maxSeek.Load() {
						maxSeek.Store(d)
					}
				}
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrFactSetBusy):
					busy.Add(1)
				default:
					t.Errorf("factset %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		heap, files := stop()
		n1, t1, max := ti.FactSetLockWaits()
		nfiles, bytes, _ := ti.FactSetFiles()
		inMem, spilled := ti.LiveFactSets()
		t.Logf("10 concurrent factsets of %d x %d (walk cap %d): %s, completed %d, refused %d, slowest far seek %s, heap peak %d MiB, live factsets in memory %d / spilled %d (factset memory now %d KiB), factset files %d (%d MiB written, %d MiB each), open factset files peak %d, session-DB lock waits %d writes total %s max-so-far %s",
			units, refs, slots, time.Since(start), ok.Load(), busy.Load(), time.Duration(maxSeek.Load()), heap>>20, inMem, spilled, sessions.FactSetMemoryUsed()>>10, nfiles, bytes>>20, (bytes/int64(max1(nfiles)))>>20, files, n1-n0, t1-t0, max)
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

// seedFlat writes n results under kb/u/ that cite nothing: unit i is one
// section, so the factset has n rows.
func seedFlat(t testing.TB, f *factsetFixture, n int) {
	t.Helper()
	for i := range n {
		f.write(t, fmt.Sprintf("kb/u/f%02d.md", i), fmt.Sprintf("F%02d", i), fmt.Sprintf("f%02d body.", i))
	}
}

// noPanic runs fn and fails the test, instead of crashing the binary, when
// it panics.
func noPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	var p any
	func() {
		defer func() { p = recover() }()
		fn()
	}()
	require.Nil(t, p, "%s PANICKED: %v", what, p)
}

// B1 (review of PR #692): a factset a call holds is never freed under it. A
// far seek (the walk of 59 units) and an export (60 rows, two chunks) hold
// their factset while the same owner's new first call REPLACES it, or the
// cursor is DELETED, in memory and after a spill. The holder never panics:
// the seek ends as the expired cursor (the 404: its row is gone), the export
// finishes whole from the factset it holds, and the factset is freed on that last
// release, its file deleted with it.
//
// Sabotage: free the factset at once in retireLocked (ignore holders), and the
// holder's next read is a nil-pointer panic; answer a vanished row with
// ErrCursorInUse in page (N4), and the seek is a 409, not the 404.
func TestFactSet_HeldFactSetSurvivesReplacementAndDelete(t *testing.T) {
	f := newFactSetFixture(t)
	seedFlat(t, f, 60)
	ti := f.svc.ToolSession()
	ctx := context.Background()
	for _, spilled := range []bool{false, true} {
		for _, action := range []string{"replace", "delete"} {
			for _, via := range []string{"seek", "export"} {
				name := fmt.Sprintf("%s-%s-spilled=%v", via, action, spilled)
				t.Run(name, func(t *testing.T) {
					if spilled {
						defer sessions.SetFactSetSpillBytesForTest(1)()
					}
					owner := "o-" + name
					req := f.request("kb/u/")
					req.Limit = 1
					p1, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
					require.NoError(t, err)
					id := *p1.Cursor
					require.Equal(t, 1, p1.UnitsWalked)
					inMem, onDisk := ti.LiveFactSets()
					before := inMem + onDisk
					acted := false
					act := func() {
						acted = true
						if action == "replace" {
							_, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
							require.NoError(t, err)
						} else {
							require.NoError(t, ti.DeleteFactSet(ctx, id))
						}
						_, ok := ti.OpenFactSet(id)
						require.False(t, ok, "a retired factset is refused to new calls")
					}
					switch via {
					case "seek":
						defer SetFactSetUnitWalkedHookForTest(func(cursor string, unit int) {
							if cursor == id && unit == 30 && !acted {
								act()
							}
						})()
						noPanic(t, "the seeking call", func() {
							_, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, id, 60)
						})
						require.ErrorIs(t, err, ErrCursorExpired, "its row is gone: the 404, never a 409")
					case "export":
						var doc strings.Builder
						noPanic(t, "the exporting call", func() {
							err = FactSetExport(ctx, f.scope(owner), f.b, nil, nil, id, func(FactSetEnv) io.Writer {
								act() // chunk 1 is read; chunk 2 is read after this
								return &doc
							})
						})
						require.NoError(t, err)
						require.Equal(t, 60, strings.Count(doc.String(), "\n## "), "the export is whole")
					}
					require.True(t, acted)
					inMem, onDisk = ti.LiveFactSets()
					want := before
					if action == "delete" {
						want--
					}
					require.Equal(t, want, inMem+onDisk, "the old factset was freed on its last release")
					require.False(t, exists(ti.FactSetFilePath(id)), "and its file deleted")
				})
			}
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// B2: an export walks the units not walked yet under a build slot, then
// gives the slot back BEFORE it streams: a stalled download holds an export
// slot (bounded apart) and never a build slot. Over the export cap, an
// export is refused at once.
//
// Sabotage: drop the releaseSlot after the walk, and the blocked export
// still holds its build slot.
func TestFactSet_ExportGivesItsBuildSlotBackBeforeStreaming(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Limit = 1
	p1, _, err := FactSetFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked, "unit 2 is left for the export to walk")
	require.Zero(t, BuildSlotsInUse())

	builds, exports := -1, -1
	err = FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p1.Cursor, func(FactSetEnv) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			if builds < 0 {
				builds, exports = BuildSlotsInUse(), ExportSlotsInUse()
			}
			return len(p), nil
		})
	})
	require.NoError(t, err)
	require.Zero(t, builds, "a streaming export holds no build slot")
	require.Equal(t, 1, exports, "it holds its export slot")
	require.Zero(t, ExportSlotsInUse(), "and gives it back at the end")

	for range MaxConcurrentFactSetExports {
		exportSlots <- struct{}{}
	}
	err = FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p1.Cursor, func(FactSetEnv) io.Writer { return io.Discard })
	for range MaxConcurrentFactSetExports {
		<-exportSlots
	}
	require.ErrorIs(t, err, ErrFactSetExportsBusy)
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// tree_depth is the depth in the WALK TREE (reading order), not the BFS
// distance (fact 33b57847): r cites a then b, a cites c, c cites d, b cites
// d. BFS puts d at distance 2 (through b); the reading order writes it under
// c, at tree depth 3, numbered 1.1.1.1. (A direct reference of the root,
// distance 1, is always the root's child in a factset: walker.pinDirect.)
//
// Sabotage: set tree_depth from the BFS depth, and d reads 2.
func TestFactSet_TreeDepthIsTheWalkTreeNotBFS(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/d.md", "D", "d body.")
	f.write(t, "kb/z/c.md", "C", "c body.", "kb/z/d.md")
	f.write(t, "kb/z/b.md", "B", "b body.", "kb/z/d.md")
	f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/c.md")
	f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/a.md", "kb/z/b.md")
	req := f.request("kb/u/")
	req.Limit = 5
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	byPath := map[string]FactSetNode{}
	for _, n := range p.Facts {
		byPath[n.Path] = n
	}
	c, d := byPath["kb/z/c.md"], byPath["kb/z/d.md"]
	require.Equal(t, 2, d.Depth, "BFS: d is two hops from r (through b)")
	require.Equal(t, 3, d.TreeDepth, "the walk tree: d is under c, under a")
	require.Equal(t, "1.1.1.1", d.Number)
	require.Equal(t, c.N, d.Parent)
	require.Equal(t, 2, c.TreeDepth)
	require.Equal(t, "1.2", byPath["kb/z/b.md"].Number)
}

// different_version on a factset link: unit 1 writes x at the version r1 cites
// (its new body); unit 2's r2 cites x's OLD version, whose body differs, so
// its back link is flagged. y was re-written with the same body (only its
// confidence changed): a different commit alone is not a change.
//
// Sabotage: never set different_version on a factset link, and x's link is
// unflagged.
func TestFactSet_LinkDifferentVersion(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/x.md", "X", "x old body.")
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/x.md", "kb/z/y.md")
	f.write(t, "kb/z/x.md", "X", "x new body.")
	ff := fact.NewFact("kb/z/y.md")
	ff.Title, ff.Body, ff.Type = "Y", "y body.", fact.Observation
	ff.Domain, ff.Entities = []string{"testing"}, []string{}
	ff.Confidence, ff.Sources = 0.9, 1
	content, err := fact.SerializeFact(ff)
	require.NoError(t, err)
	_, err = f.svc.Facts().WriteFact(context.Background(), "agent/test", "kb/z/y.md", content, "y again", "")
	require.NoError(t, err)
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/x.md", "kb/z/y.md")

	req := f.request("kb/u/")
	req.Limit = 5
	ctx := context.Background()
	p, _, err := FactSetFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	nodes := p.Facts
	for p.HasMore {
		p, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		nodes = append(nodes, p.Facts...)
	}
	links := map[string]*FactSetLink{}
	for _, n := range nodes {
		if n.Unit == 2 && n.Link != nil {
			links[n.Path] = n.Link
		}
	}
	require.Equal(t, "kb/u/r1.md", nodes[0].Path, "r1 is unit 1 (newest)")
	require.NotNil(t, links["kb/z/x.md"])
	require.True(t, links["kb/z/x.md"].DifferentVersion, "r2 cites x at a version with another body")
	require.NotNil(t, links["kb/z/y.md"])
	require.False(t, links["kb/z/y.md"].DifferentVersion, "same body at another commit is not a change")
}

// N3: a result the search listed that is gone at the anchor (deleted
// between the search and the HEAD the factset is walked at) keeps its unit, as
// a heading alone under the title the search listed: never a 503 "retry",
// which every retry at the frozen anchor would answer again. Pages and the
// export both serve it.
//
// Sabotage: make the gone result's walk error (the old ErrBookChanged 503), and
// the unit is refused.
func TestFactSet_ResultGoneAtTheAnchorIsAHeadingUnit(t *testing.T) {
	f := newFactSetFixture(t)
	fixtureTwo(t, f)
	defer f.svc.Search().SetOverridesForTest(index.Overrides{Search: func(next index.SearchFunc) index.SearchFunc {
		return func(ctx context.Context, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
			out, err := next(ctx, branch, q)
			gone := index.SearchResult{FactWithBody: facts.FactWithBody{FactRecord: facts.FactRecord{Path: "kb/u/gone.md", Title: "Gone fact"}}}
			return append(out, gone), err
		}
	}})()
	req := f.request("kb/u/")
	req.Limit = 5
	ctx := context.Background()
	p, _, err := FactSetFirstPage(ctx, f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 3, p.Units, "the gone result keeps its unit")
	nodes := p.Facts
	for p.HasMore {
		p, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		nodes = append(nodes, p.Facts...)
	}
	last := nodes[len(nodes)-1]
	require.Equal(t, 3, last.Unit)
	require.Equal(t, "kb/u/gone.md", last.Path)
	require.True(t, last.Unavailable)
	require.False(t, last.Written)
	require.Nil(t, last.Link)
	require.Equal(t, "Gone fact", last.Title)
	require.Equal(t, "3", last.Number)

	var doc strings.Builder
	require.NoError(t, FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return &doc }))
	require.Contains(t, doc.String(), "## 3. Gone fact "+UnavailableNote)
}

// seedABC writes the two-level reservation example the user approved:
// results A, B, C (units 1, 2, 3) under kb/u/, and under kb/z/
//
//	A -> X, Y; B -> Y, Z, C; C -> W, X; X -> P, Z; Y -> Q; Z -> P, S;
//	W -> Q, R; Q -> R; P, R, S leaves.
//
// C is written before B and A, so the search lists a, b, c (newest first,
// and by path on a tie).
func seedABC(t testing.TB, f *factsetFixture) {
	t.Helper()
	for _, leaf := range []string{"p", "r", "s"} {
		f.write(t, "kb/z/"+leaf+".md", strings.ToUpper(leaf), leaf+" body.")
	}
	f.write(t, "kb/z/q.md", "Q", "q body.", "kb/z/r.md")
	f.write(t, "kb/z/z.md", "Z", "z body.", "kb/z/p.md", "kb/z/s.md")
	f.write(t, "kb/z/w.md", "W", "w body.", "kb/z/q.md", "kb/z/r.md")
	f.write(t, "kb/z/y.md", "Y", "y body.", "kb/z/q.md")
	f.write(t, "kb/z/x.md", "X", "x body.", "kb/z/p.md", "kb/z/z.md")
	f.write(t, "kb/u/c.md", "C", "c body.", "kb/z/w.md", "kb/z/x.md")
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/y.md", "kb/z/z.md", "kb/u/c.md")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/x.md", "kb/z/y.md")
}

// readAll pages a factset from page to its end, every node in factset order.
func readAll(t *testing.T, f *factsetFixture, owner string, p FactSetPage) []FactSetNode {
	t.Helper()
	nodes := append([]FactSetNode(nil), p.Facts...)
	for p.HasMore {
		var err error
		p, err = FactSetNextPage(context.Background(), f.scope(owner), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		nodes = append(nodes, p.Facts...)
	}
	return nodes
}

// row is one expected appearance: its number, fact, and either written or a
// link (direction and the target's number).
type row struct {
	number, fact string
	link         string // "" written; "forward 2.2", "back 1.2"
}

func rowsOf(nodes []FactSetNode) []row {
	out := make([]row, len(nodes))
	for i, n := range nodes {
		r := row{number: n.Number, fact: strings.TrimSuffix(strings.TrimPrefix(n.Path, "kb/"), ".md")}
		if n.Link != nil {
			r.link = n.Link.Direction + " " + n.Link.Number
		}
		out[i] = r
	}
	return out
}

// Two-level reservation (user ruling 2026-10-29), the user's own example at
// Depth 3, References unlimited: before any unit is walked, the results
// AND each result's direct references are reserved (X, Y -> unit 1, Z ->
// unit 2, W -> unit 3; a direct reference shared by two results goes to the
// earlier one), so unit 1's deep walk links FORWARD to Z at its fixed
// number 2.2 instead of writing it. Every number, direction and target is
// asserted, and every link's anchor is the written appearance's.
//
// Sabotage: reserve only the results (the old seeded set), and 1.1.2 Z is
// written in unit 1; drop pinDirect, and a direct reference nests under a
// sibling; number reservations from 0, and (see 2.2) points nowhere.
func TestFactSet_TwoLevelReservationExample(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 3, p.Units)
	nodes := readAll(t, f, "o", p)
	require.Equal(t, []row{
		{"1", "u/a", ""},
		{"1.1", "z/x", ""},
		{"1.1.1", "z/p", ""},
		{"1.1.2", "z/z", "forward 2.2"},
		{"1.2", "z/y", ""},
		{"1.2.1", "z/q", ""},
		{"1.2.1.1", "z/r", ""},
		{"2", "u/b", ""},
		{"2.1", "z/y", "back 1.2"},
		{"2.2", "z/z", ""},
		{"2.2.1", "z/p", "back 1.1.1"},
		{"2.2.2", "z/s", ""},
		{"2.3", "u/c", "forward 3"},
		{"3", "u/c", ""},
		{"3.1", "z/w", ""},
		{"3.1.1", "z/q", "back 1.2.1"},
		{"3.1.2", "z/r", "back 1.2.1.1"},
		{"3.2", "z/x", "back 1.1"},
	}, rowsOf(nodes))

	written := map[string]FactSetNode{}
	for _, n := range nodes {
		if n.Written {
			require.NotContains(t, written, n.Path, "%s written once", n.Path)
			written[n.Path] = n
		}
	}
	for _, n := range nodes {
		if n.Link == nil {
			continue
		}
		w, ok := written[n.Path]
		require.True(t, ok, "%s is linked to and written", n.Path)
		require.Equal(t, w.AnchorID, n.Link.AnchorID, "%s %s targets the written appearance", n.Number, n.Path)
		require.Equal(t, w.Number, n.Link.Number)
		require.Equal(t, w.Unit, n.Link.Unit)
		if n.Link.Direction == LinkBack || n.Link.Seq != 0 {
			// A forward link served before its unit was walked has no seq
			// yet (the contract); every other link names the target's.
			require.Equal(t, w.Seq, n.Link.Seq, "%s: the target's seq", n.Number)
		}
	}

	bk, ok := f.svc.ToolSession().OpenFactSet(*p.Cursor)
	require.True(t, ok)
	defer bk.Close()
	reservedAs := func(path string) string {
		r, ok, err := bk.Reserved(context.Background(), path)
		require.NoError(t, err)
		if !ok {
			return ""
		}
		return r.Number
	}
	require.Equal(t, "1.1", reservedAs("kb/z/x.md"), "X: A and C cite it directly, A is earlier")
	require.Equal(t, "1.2", reservedAs("kb/z/y.md"), "Y: A and B cite it directly, A is earlier")
	require.Equal(t, "2.2", reservedAs("kb/z/z.md"))
	require.Equal(t, "3.1", reservedAs("kb/z/w.md"))
	for _, below := range []string{"kb/z/p.md", "kb/z/q.md", "kb/z/r.md", "kb/z/s.md", "kb/u/a.md", "kb/u/b.md", "kb/u/c.md"} {
		require.Empty(t, reservedAs(below), "%s: below the direct references, or a result: not reserved", below)
	}
}

// The reservation reserves only what the owner can show under its own
// limits: with References = 1 (max_nodes 2) each unit shows one direct
// reference, so A reserves X, B reserves Y (A cannot show it), and C
// reserves only W (its second, X, is beyond its one); at Depth 0 nothing is
// reserved. Every reserved fact is written by its owner.
//
// Sabotage: ignore max_nodes when reserving, and Y is reserved for unit 1,
// which never writes it: unit 2 then links to a Y written nowhere.
func TestFactSet_ReservationFollowsTheOwnersLimits(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	reservations := func(owner string, depth, maxNodes int) (map[string]string, []FactSetNode) {
		req := f.request("kb/u/")
		req.Options.Depth, req.Options.MaxNodes, req.Limit = depth, maxNodes, 5
		p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
		require.NoError(t, err)
		nodes := readAll(t, f, owner, p)
		bk, ok := f.svc.ToolSession().OpenFactSet(*p.Cursor)
		require.True(t, ok)
		defer bk.Close()
		out := map[string]string{}
		for _, path := range []string{"kb/z/x.md", "kb/z/y.md", "kb/z/z.md", "kb/z/w.md", "kb/z/p.md", "kb/z/q.md"} {
			r, ok, err := bk.Reserved(ctx, path)
			require.NoError(t, err)
			if ok {
				out[path] = r.Number
			}
		}
		return out, nodes
	}
	one, nodes := reservations("refs1", 3, 2)
	require.Equal(t, map[string]string{"kb/z/x.md": "1.1", "kb/z/y.md": "2.1", "kb/z/w.md": "3.1"}, one)
	require.Equal(t, []row{
		{"1", "u/a", ""}, {"1.1", "z/x", ""},
		{"2", "u/b", ""}, {"2.1", "z/y", ""},
		{"3", "u/c", ""}, {"3.1", "z/w", ""},
	}, rowsOf(nodes))

	zero, nodes := reservations("depth0", 0, MaxFactSetFacts)
	require.Empty(t, zero, "Depth 0 reserves nothing")
	require.Equal(t, []row{{"1", "u/a", ""}, {"2", "u/b", ""}, {"3", "u/c", ""}}, rowsOf(nodes))
}

// A unit's direct references are its root's children, in refs order, even
// when one is also reachable through a sibling: r cites a then b, a cites
// b. The plain reading order would nest b under a (1.1.1); in a factset b is
// 1.2, the number reserved for it before the walk, and a's builds_on still
// points at it.
//
// Sabotage: drop walker.pinDirect, and b is walked as 1.1.1 against its
// reserved 1.2 (the unit is refused).
func TestFactSet_DirectRefsAreTheRootsChildren(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/b.md", "B", "b body.")
	f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/b.md")
	f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/a.md", "kb/z/b.md")
	req := f.request("kb/u/")
	req.Limit = 5
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, []row{{"1", "u/r", ""}, {"1.1", "z/a", ""}, {"1.2", "z/b", ""}}, rowsOf(p.Facts))
	a, b := p.Facts[1], p.Facts[2]
	require.Equal(t, 1, b.TreeDepth)
	require.Len(t, a.BuildsOn, 1)
	require.Equal(t, b.AnchorID, a.BuildsOn[0].AnchorID)
}

// A seek to unit 3 before units 2 and 3 are walked serves exactly what
// reading the factset from the start does: the reservations are fixed on the
// first call.
//
// Sabotage: compute the reservations lazily per walked unit, and unit 3
// differs.
func TestFactSet_SeekGivesTheSameReservedFactSet(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	p, _, err := FactSetFirstPage(ctx, f.scope("whole"), req)
	require.NoError(t, err)
	var unit3 []FactSetNode
	for _, n := range readAll(t, f, "whole", p) {
		if n.Unit == 3 {
			unit3 = append(unit3, n)
		}
	}

	req.Limit = 1
	p1, _, err := FactSetFirstPage(ctx, f.scope("seek"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p1.UnitsWalked)
	p3, err := FactSetNextPage(ctx, f.scope("seek"), f.b, nil, nil, *p1.Cursor, 3)
	require.NoError(t, err)
	seeked := append([]FactSetNode(nil), p3.Facts...)
	for p3.HasMore {
		p3, err = FactSetNextPage(ctx, f.scope("seek"), f.b, nil, nil, *p3.Cursor, 0)
		require.NoError(t, err)
		seeked = append(seeked, p3.Facts...)
	}
	strip := func(ns []FactSetNode) []FactSetNode {
		out := make([]FactSetNode, len(ns))
		for i, n := range ns {
			n.Date = ""
			out[i] = n
		}
		return out
	}
	require.Equal(t, strip(unit3), strip(seeked))
}

// The batched read changes nothing when a cited version has NO index row
// (a historical blob the index never saw: on knomit-kb, 416 of page 1's
// reads): the batch reports it as a miss and the walk reads its blob, as
// the per-reference path does. The facts row of x's old version is deleted
// to make the miss; the page stats prove the blob fallback ran.
//
// Sabotage: let the prefetch record a miss as a hit, and r1's x is titled
// "" from an empty row instead of read from its blob.
func TestFactSet_BatchedReadsSameOnAnIndexMiss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.db")
	svc, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	})
	f := &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
	f.write(t, "kb/z/x.md", "X old", "x old body.")
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/u/r1.md", "R1", "r1 body.", "kb/z/x.md", "kb/z/y.md")
	f.write(t, "kb/z/x.md", "X new", "x new body.")
	f.write(t, "kb/u/r2.md", "R2", "r2 body.", "kb/z/x.md", "kb/z/y.md")

	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer raw.Close()
	res, err := raw.Exec(`DELETE FROM facts WHERE path = 'kb/z/x.md' AND title = 'X old'`)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.EqualValues(t, 1, n, "x's old version is no longer indexed")

	read := func(owner string) ([]string, int) {
		req := f.request("kb/u/")
		req.Limit = 5
		p, st, err := FactSetFirstPage(ctx, f.scope(owner), req)
		require.NoError(t, err)
		parsed := st.Walk.Parsed
		var out []string
		for {
			b, err := json.Marshal(p)
			require.NoError(t, err)
			out = append(out, strings.ReplaceAll(string(b), *p.Cursor, "CURSOR"))
			if !p.HasMore {
				return out, parsed
			}
			var st2 FactSetBuildStats
			p, st2, err = FactSetNextPageStats(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, 0)
			require.NoError(t, err)
			parsed += st2.Walk.Parsed
		}
	}
	batched, parsedB := read("batched")
	defer SetVersionPrefetchForTest(false)()
	perRef, parsedP := read("per-ref")
	require.Equal(t, perRef, batched)
	require.Positive(t, parsedB, "the old x was read from its blob")
	require.Equal(t, parsedP, parsedB)
	require.Contains(t, strings.Join(batched, ""), `"title":"X old"`)
}

// The batched version read (index.VersionMetasAt, round 4) answers every
// (path, commit) exactly as the per-pair VersionMetaAt does, hits and
// misses alike: an old version, the current one, a version before the fact
// existed, a directory, a mixed-case spelling, an unknown commit, a deeper
// path, and a pair asked twice.
//
// Sabotage: resolve a directory's hash as a hit, or answer a pair from the
// wrong commit's tree, and a pair differs.
func TestVersionMetasAt_IsVersionMetaAtForEveryPair(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	write := func(path, title, body string, refs ...string) string {
		ff := fact.NewFact(path)
		ff.Title, ff.Body, ff.Type = title, body, fact.Observation
		ff.Domain, ff.Entities, ff.Refs = []string{"testing"}, []string{}, refs
		ff.Confidence, ff.Sources = 0.7, 1
		content, err := fact.SerializeFact(ff)
		require.NoError(t, err)
		r, err := f.svc.Facts().WriteFact(ctx, "agent/test", path, content, "write "+path, "")
		require.NoError(t, err)
		return r.CommitHash
	}
	c0 := write("kb/z/a.md", "A", "a body.")
	c1 := write("kb/z/x.md", "X", "x old.", "kb/z/a.md")
	c2 := write("kb/z/x.md", "X2", "x new.")
	c3 := write("kb/z/deep/y.md", "Y", "y body.", "kb/z/x.md", "kb/z/a.md")
	pairs := []index.PathAtCommit{
		{Path: "kb/z/x.md", Commit: c1}, {Path: "kb/z/x.md", Commit: c2}, {Path: "kb/z/x.md", Commit: c0},
		{Path: "kb/z", Commit: c2}, {Path: "kb/z/X.md", Commit: c2}, {Path: "kb/z/x.md", Commit: strings.Repeat("0", 40)},
		{Path: "kb/z/deep/y.md", Commit: c3}, {Path: "kb/z/a.md", Commit: c3}, {Path: "kb/z/x.md", Commit: c1},
		{Path: "kb/nope/q.md", Commit: c3}, {Path: "kb/z/deep", Commit: c3},
	}
	got, err := f.svc.FactQuery().VersionMetasAt(ctx, pairs)
	require.NoError(t, err)
	hits := 0
	for _, p := range pairs {
		want, ok, err := f.svc.FactQuery().VersionMetaAt(ctx, p.Path, p.Commit)
		require.NoError(t, err)
		g, gok := got[p]
		require.Equal(t, ok, gok, "%s@%s: hit", p.Path, p.Commit[:8])
		require.Equal(t, want, g, "%s@%s: row", p.Path, p.Commit[:8])
		if ok {
			hits++
		}
	}
	require.Equal(t, 5, hits, "x@c1, x@c2, y@c3, a@c3 and x@c1 again are hits; the rest miss")
	require.Equal(t, "X", got[index.PathAtCommit{Path: "kb/z/x.md", Commit: c1}].Title, "the old version's row")
	require.Equal(t, "X2", got[index.PathAtCommit{Path: "kb/z/x.md", Commit: c2}].Title)
}

// The BFS takes nodes in queue order when max_nodes cuts inside a level: r
// cites a then b, a cites x, b cites y; at max_nodes 4 the walk includes r,
// a, b and x (a was queued before b) and cuts y. The level-by-level form
// (round 4, for batched reads) must keep that FIFO order exactly.
//
// Sabotage: walk a level in reverse, and y is included instead of x.
func TestPlan_SizeCutFollowsTheQueueOrder(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/x.md", "X", "x body.")
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/x.md")
	f.write(t, "kb/z/b.md", "B", "b body.", "kb/z/y.md")
	f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/a.md", "kb/z/b.md")
	doc, err := Plan(context.Background(), Request{Binding: f.b, File: "kb/u/r.md", Options: Options{Depth: 3, MaxNodes: 4}})
	require.NoError(t, err)
	var paths []string
	cut := map[string]string{}
	for _, n := range doc.Nodes {
		paths = append(paths, n.Path)
		for _, o := range n.NotIncluded {
			cut[o.Path] = o.Reason
		}
	}
	require.Equal(t, []string{"kb/u/r.md", "kb/z/a.md", "kb/z/x.md", "kb/z/b.md"}, paths)
	require.Equal(t, ReasonSize, cut["kb/z/y.md"])
}
