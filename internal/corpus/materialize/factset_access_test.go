package materialize

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedSixteen writes 16 results (kb/u/r000..r013, r900, r901) whose own refs
// share four leaves, one fact cited at two versions (t at c1 and at c3, one
// blob), so later units link back to facts earlier units wrote, at the same
// and at another version.
func seedSixteen(t testing.TB, f *factsetFixture) {
	t.Helper()
	f.write(t, "kb/z/t.md", "T", "t v1.")
	f.write(t, "kb/u/r900.md", "R900", "cites t at v1.", "kb/z/t.md")
	f.write(t, "kb/z/t.md", "T", "t v2.")
	f.write(t, "kb/u/r901.md", "R901", "cites t at v2.", "kb/z/t.md")
	for _, l := range []string{"a", "b", "c", "d"} {
		f.write(t, "kb/z/"+l+".md", strings.ToUpper(l), l+" body.", "kb/z/t.md")
	}
	for i := range 14 {
		n := leftPad(i)
		f.write(t, "kb/y/o"+n+".md", "O"+n, "own body.", "kb/z/"+string(rune('a'+i%4))+".md")
		f.write(t, "kb/u/r"+n+".md", "R"+n, "r body.", "kb/y/o"+n+".md", "kb/z/"+string(rune('a'+(i+1)%4))+".md")
	}
}

// sequentialPages pages a FactSet from its first page to its end and returns
// each page's JSON keyed by the cursor that served it ("" for page 1), and
// the cursors in reading order.
func sequentialPages(t testing.TB, f *factsetFixture, owner string, p FactSetPage) (map[string]string, []string) {
	t.Helper()
	byCursor := map[string]string{"": mustJSON(t, p)}
	var order []string
	for p.HasMore {
		cur := *p.Cursor
		var err error
		p, err = FactSetNextPage(context.Background(), f.scope(owner), f.b, nil, nil, cur, 0)
		require.NoError(t, err)
		byCursor[cur] = mustJSON(t, p)
		order = append(order, cur)
	}
	return byCursor, order
}

// A page is a function of (FactSet, position): every position a sequential
// read serves is served byte for byte the same when read in reverse, in a
// random order, by eight concurrent readers on a cold cache racing to extend
// the prefix, and by seeks to each unit; the export after all of it equals
// the export of a fresh FactSet. Pinned and unpinned, on the two-level
// reservation example and on a store whose later units cite facts earlier
// units wrote (at the same and at another version).
//
// Sabotage: let lookup count every written fact, not only those written
// before the unit (backward reads turn first appearances into back links);
// report the cache's extent as UnitsWalked (pages differ after a far read).
func TestFactSet_RandomAccessIsByteIdentical(t *testing.T) {
	type tc struct {
		name  string
		seed  func(testing.TB, *factsetFixture)
		depth int
		unpin bool
		limit int
	}
	for _, c := range []tc{
		{"abc", seedABC, 3, false, 1},
		{"abc-limit2", seedABC, 3, false, 2},
		{"sixteen", seedSixteen, 3, false, 1},
		{"sixteen-unpin", seedSixteen, 3, true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFactSetFixture(t)
			c.seed(t, f)
			ctx := context.Background()
			req := f.request("kb/u/")
			req.Options.Depth, req.Options.Unpin, req.Limit = c.depth, c.unpin, c.limit
			DropFactSetCacheForTest()
			p := f.first(t, "o", req)
			want, order := sequentialPages(t, f, "o", p)
			require.Greater(t, len(order), p.Units, "more pages than units")
			var fresh bytes.Buffer
			require.NoError(t, FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return &fresh }))

			read := func(owner, cur string) string {
				got, err := FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, cur, 0)
				require.NoError(t, err)
				return mustJSON(t, got)
			}
			// Reverse, from a cold cache.
			DropFactSetCacheForTest()
			for i := len(order) - 1; i >= 0; i-- {
				require.Equal(t, want[order[i]], read("o", order[i]), "reverse: %s", order[i])
			}
			// Random order, cold cache, three seeds.
			for seed := int64(1); seed <= 3; seed++ {
				DropFactSetCacheForTest()
				perm := rand.New(rand.NewSource(seed)).Perm(len(order))
				for _, i := range perm {
					require.Equal(t, want[order[i]], read("o", order[i]), "seed %d: %s", seed, order[i])
				}
			}
			// Eight concurrent readers, each its own random order, cold cache.
			DropFactSetCacheForTest()
			var wg sync.WaitGroup
			type miss struct{ cur, got string }
			misses := make(chan miss, 8*len(order))
			errs := make(chan error, 8*len(order))
			for r := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					perm := rand.New(rand.NewSource(int64(100 + r))).Perm(len(order))
					for _, i := range perm {
						got, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, order[i], 0)
						if err != nil {
							errs <- err
							return
						}
						if s := mustJSON(t, got); s != want[order[i]] {
							misses <- miss{order[i], s}
						}
					}
				}()
			}
			wg.Wait()
			close(misses)
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}
			for m := range misses {
				require.Equal(t, want[m.cur], m.got, "concurrent: %s", m.cur)
			}
			// Seeks: a seek to unit u is the page at (u, 0).
			DropFactSetCacheForTest()
			id := mustID(t, *p.Cursor)
			for u := p.Units; u >= 1; u-- {
				at := cursorToken(id, fpos{u, 0})
				if _, ok := want[at]; !ok {
					continue // not a page boundary at this limit
				}
				got, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, u)
				require.NoError(t, err)
				require.Equal(t, want[at], mustJSON(t, got), "seek to unit %d", u)
			}
			var after bytes.Buffer
			require.NoError(t, FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return &after }))
			require.Equal(t, fresh.String(), after.String(), "the export does not depend on what was read before")
		})
	}
}

// A backward seek after a far seek re-plans the one unit it needs, never the
// units before it; a unit whose plan is kept plans nothing; nothing misses
// the cache.
//
// Sabotage: reset the prefix on a backward seek (the old replay): the seek
// to unit 10 plans 10 units.
func TestFactSet_BackwardSeekDoesNotReplay(t *testing.T) {
	f := newFactSetFixture(t)
	seedSixteen(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 1
	DropFactSetCacheForTest()
	p := f.first(t, "o", req)
	require.Equal(t, 16, p.Units)
	seek := func(u int) Stats {
		t.Helper()
		_, st, err := FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, u)
		require.NoError(t, err)
		require.Zero(t, st.CacheMisses, "unit %d", u)
		return st
	}
	require.Equal(t, 15, seek(16).UnitsPlanned, "the far seek extends the prefix through unit 16")
	require.Equal(t, 1, seek(3).UnitsPlanned, "backward: unit 3 alone")
	require.Equal(t, 1, seek(5).UnitsPlanned, "backward: one unit, not the units before it")
	require.Equal(t, 0, seek(16).UnitsPlanned, "unit 16's plan is kept")
	require.Equal(t, 0, seek(3).UnitsPlanned, "unit 3's plan is kept now")
}

// Calls on one FactSet run in parallel: while an extension is blocked inside
// planning (holding the extension token), reads of every planned unit are
// served; a call that needs the extension waits for it and, cancelled,
// returns its context's error at once; once the extension proceeds, the
// waiter's twin is served.
//
// Sabotage: make a read of a planned unit take the extension token (calls
// serialize): the reads wait behind the blocked extension and time out.
func TestFactSet_ReadsRunBesideABlockedExtension(t *testing.T) {
	f := newFactSetFixture(t)
	seedSixteen(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 1
	DropFactSetCacheForTest()
	p := f.first(t, "o", req)
	seekWant := map[int]string{}
	for u := 1; u <= p.Units; u++ {
		got, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, u)
		require.NoError(t, err)
		seekWant[u] = mustJSON(t, got)
	}
	DropFactSetCacheForTest()
	_, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 12) // prefix: units 1..12, plans of 5..12 kept
	require.NoError(t, err)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer SetFactSetExtendHookForTest(func(unit int) {
		if unit == 13 {
			once.Do(func() { close(entered) })
			<-release
		}
	})()
	extended := make(chan error, 1)
	go func() {
		_, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 16)
		extended <- err
	}()
	<-entered

	replanned := 0
	for u := 1; u <= 12; u++ {
		done := make(chan string, 1)
		go func() {
			got, st, err := FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, u)
			if err != nil {
				done <- "error: " + err.Error()
				return
			}
			if u <= 4 && st.UnitsPlanned > 0 {
				replanned++
			}
			done <- mustJSON(t, got)
		}()
		select {
		case got := <-done:
			require.Equal(t, seekWant[u], got, "unit %d read beside the blocked extension", u)
		case <-time.After(5 * time.Second):
			t.Fatalf("unit %d: a read of a planned unit waited for the blocked extension", u)
		}
	}
	require.Equal(t, 4, replanned, "units 1-4 (not kept) were re-planned, beside the extension")

	wctx, cancel := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() {
		_, err := FactSetNextPage(wctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 15)
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("unit 15 needs the blocked extension, yet the call returned: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waited:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter did not return promptly")
	}

	close(release)
	require.NoError(t, <-extended)
	got, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 15)
	require.NoError(t, err)
	require.Equal(t, seekWant[15], mustJSON(t, got))
}

// No slot count: twelve first calls at once (three times the old four
// planning slots) all succeed. Over the memory budget a call that must plan
// WAITS: it holds on while another plan holds the memory, returns its
// context's error at once when cancelled, ErrFactSetBusy only past the
// budget wait, and is served as soon as the memory is given back.
//
// Sabotage: refuse at once over the budget (the waiter returns before it is
// cancelled, and the call freed mid-wait is refused); admit over the budget
// (the waiter is served while the memory is held).
func TestFactSet_PlanningWaitsForMemoryInsteadOfRefusing(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 1

	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = FactSetFirstPage(ctx, f.scope("c"+itoa(int32(i))), req)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "first call %d", i)
	}
	require.Zero(t, FactSetPlansInFlightForTest())

	defer SetFactSetCacheBudget(0)
	SetFactSetCacheBudget(1)
	release := HoldFactSetPlanningMemoryForTest(1)
	wctx, cancel := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() {
		_, _, err := FactSetFirstPage(wctx, f.scope("waiter"), req)
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("the budget is full, yet the first call returned: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waited:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter did not return promptly")
	}

	restore := SetFactSetBudgetWaitForTest(50 * time.Millisecond)
	_, _, err := FactSetFirstPage(ctx, f.scope("busy"), req)
	restore()
	require.True(t, errors.Is(err, ErrFactSetBusy), "past the budget wait: %v", err)

	go func() { time.Sleep(100 * time.Millisecond); release() }()
	_, _, err = FactSetFirstPage(ctx, f.scope("freed"), req)
	require.NoError(t, err, "served once the memory is given back")
	require.Zero(t, FactSetPlansInFlightForTest())
}
