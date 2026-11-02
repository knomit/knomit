package materialize

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// A member the search does not read is not waited on (review M1). gamma has
// synced once, and a sync lands on it during EVERY fan-out (the search hook
// writes to it), so whenever gamma is sandwiched the call is busy:
//   - the plain lens FactSet searches gamma and is busy (the control);
//   - narrowed by repo= to alpha and beta, the call answers, and env.anchors
//     holds alpha and beta only (a member not searched has no anchor);
//   - under since_fork only the write member is searched: it answers, anchors
//     alpha alone, and gamma stays an empty sub with no anchor.
//
// Sabotage (sandwich all members: open every Reads() mount into the sandwich
// whatever the plan searches): the narrowed calls are busy.
func TestLensFactSet_UnsearchedUnsettledMemberIsNotWaitedOn(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.w.write(t, "kb/u/w1.md", "W1", cosBody(0.95, "w1."))
	lt.r1.write(t, "kb/u/a1.md", "A1", cosBody(0.9, "a1."))
	defer SetFactSetAnchorWaitForTest(200 * time.Millisecond)()
	var n atomic.Int64
	defer SetFactSetSearchHookForTest(func() {
		lt.r2.write(t, "kb/v/s"+strconv.FormatInt(n.Add(1), 10)+".md", "S", "a sync on gamma.")
	})()
	ctx := context.Background()

	_, _, err := FactSetFirstPage(ctx, lt.scope(), lt.request("anything", "kb/u/"))
	require.ErrorIs(t, err, ErrIndexMoved, "control: gamma is searched and keeps moving")

	req := lt.request("anything", "kb/u/")
	req.Search.Repos = []string{"alpha", "beta"}
	p, _, err := FactSetFirstPage(ctx, lt.scope(), req)
	require.NoError(t, err, "gamma is narrowed out: it is not waited on")
	require.Equal(t, 2, p.Units)
	require.ElementsMatch(t, []string{lt.w.id12(), lt.r1.id12()}, sortedKeys(p.Anchors), "anchors of the searched members only")

	req = lt.request("anything", "kb/u/")
	req.Search.SinceFork, req.PathsIn = true, []string{"kb/u/w1.md"}
	p, _, err = FactSetFirstPage(ctx, lt.scope(), req)
	require.NoError(t, err, "under since_fork only the write member is searched")
	require.Equal(t, 1, p.Units)
	require.Equal(t, []string{lt.w.id12()}, sortedKeys(p.Anchors))
	lf := lensFrozenOf(t, lt.scope(), *p.Cursor)
	require.Len(t, lf.Subs, 3)
	require.Empty(t, lf.Subs[2].Anchor)
	require.Empty(t, lf.Subs[2].Roots)
	_, err = FactSetNextPage(ctx, lt.scope(), lt.lens, nil, nil, *p.Cursor, 1)
	require.NoError(t, err, "it pages")
}

// A lens member whose index has NEVER synced is EMPTY (user ruling,
// 2026-11-01): gamma was initialised but never synced (empty watermark, no
// rebuild generation). The plain no-filter lens FactSet ("All in lens")
// answers at once (no wait), with:
//   - no root from gamma, and gamma's search leg never run (it is dropped
//     from the targets before the fan-out);
//   - gamma absent from env.anchors and an empty sub in the row;
//   - a ref into gamma still a not-followed leaf with gamma's name.
//
// A member that WAS settled and then syncs during the fan-out still makes the
// call busy: TestLensFactSet_SyncOnR2DuringTheFanOutIsBusy and the control of
// TestLensFactSet_UnsearchedUnsettledMemberIsNotWaitedOn.
//
// Sabotage (neverSynced always false: gamma is searched and waited on): the
// call is ErrIndexMoved.
func TestLensFactSet_NeverSyncedMemberIsEmpty(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := &lensTrio{w: newLensMember(t, "alpha"), r1: newLensMember(t, "beta"), r2: newUnsyncedLensMember(t, "gamma")}
	lt.lens = repos.NewBindingForTest(lt.w.ri,
		repos.ReadTarget{RI: lt.w.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r1.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r2.ri, Branch: "agent/test"})
	w, gen, settled, err := lt.r2.svc.HistoryQuery().SettledWatermark(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, [3]any{"", "", false}, [3]any{w, gen, settled}, "the fixture: gamma has never synced")
	gammaX := wireOf(lt.lens, lt.r2.ri, "kb/z/x.md")
	lt.w.write(t, "kb/u/w1.md", "W1", "w1.", gammaX)
	lt.r1.write(t, "kb/u/a1.md", "A1", "a1.")
	defer SetFactSetAnchorWaitForTest(200 * time.Millisecond)()

	req := lt.request("", "")
	req.Search.Sort = SortRecent
	start := time.Now()
	p, _, err := FactSetFirstPage(context.Background(), lt.scope(), req)
	require.NoError(t, err, "a never-synced member is empty, not waited on")
	require.Less(t, time.Since(start), 200*time.Millisecond, "no wait for gamma")
	require.Equal(t, "All in lens alpha", p.Title)
	roots := unitRoots(allPages(t, lt.scope(), lt.lens, p)...)
	for _, n := range roots {
		require.NotEqual(t, lt.r2.id12(), n.Repo, "gamma contributes no root: %s", n.Path)
	}
	require.ElementsMatch(t, []string{"kb/u/w1.md", "kb/seed/alpha.md", wireOf(lt.lens, lt.r1.ri, "kb/u/a1.md"), wireOf(lt.lens, lt.r1.ri, "kb/seed/beta.md")}, paths(roots))
	require.ElementsMatch(t, []string{lt.w.id12(), lt.r1.id12()}, sortedKeys(p.Anchors), "gamma has no anchor")
	lf := lensFrozenOf(t, lt.scope(), *p.Cursor)
	require.Equal(t, "gamma", lf.Subs[2].Name)
	require.Empty(t, lf.Subs[2].Anchor)
	require.Empty(t, lf.Subs[2].Roots)
	var leaf *Omission
	for _, n := range roots {
		if n.Path == "kb/u/w1.md" {
			for i, o := range n.NotIncluded {
				if o.Path == gammaX {
					leaf = &n.NotIncluded[i]
				}
			}
		}
	}
	require.NotNil(t, leaf, "the ref into gamma is still listed")
	require.Equal(t, Omission{Path: gammaX, Reason: ReasonLensRepo, RepoName: "gamma"}, *leaf)
}

// S5 (wording ruling): the search line spells a lens's narrowing "in <names>",
// comma-separated.
func TestSearchLine_LensNarrowing(t *testing.T) {
	require.Equal(t, "in alpha, beta · sorted by recent", SearchLine(FactSetSearch{Repos: []string{"alpha", "beta"}, Sort: SortRecent}))
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Resume refuses a row that disagrees with the binding (review L2), each as
// the expired cursor, never a 500:
//   - two subs of one repo (decodeLensFrozen);
//   - a wire repo that is not the binding's write repo (attachLens);
//   - a read member pinned at another branch than the row froze (attachLens).
//
// The control attaches the row as stored over the lens it was made on.
//
// Sabotage, one run each: drop the unique-repo check; drop the wire check;
// drop the branch check.
func TestLensFactSet_ResumeRefusesARowThatDisagreesWithTheBinding(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	p := lt.first(t, lt.request("anything", "kb/u/"))
	lf := lensFrozenOf(t, lt.scope(), *p.Cursor)
	attach := func(b *repos.Binding, lf *lensFrozen) error {
		c := &fsCall{ctx: context.Background(), b: b}
		c.r = newReader(b, nil, nil, &c.st)
		defer c.close()
		return c.attachLens(newLensEntry("x", nil, lf))
	}
	require.NoError(t, attach(lt.lens, lf), "control: the row over its own lens")

	dup := *lf
	dup.Subs = append([]lensSub(nil), lf.Subs...)
	dup.Subs[2].Repo = dup.Subs[1].Repo
	raw, err := json.Marshal(&dup)
	require.NoError(t, err)
	_, err = decodeLensFrozen(raw)
	require.ErrorIs(t, err, ErrCursorExpired, "two subs of one repo")

	wire := *lf
	wire.WireRepo = lt.r1.id12()
	require.ErrorIs(t, attach(lt.lens, &wire), ErrCursorExpired, "a wire repo that is not the write repo")

	moved := repos.NewBindingForTest(lt.w.ri,
		repos.ReadTarget{RI: lt.w.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r1.ri, Branch: "other"},
		repos.ReadTarget{RI: lt.r2.ri, Branch: "agent/test"})
	require.ErrorIs(t, attach(moved, lf), ErrCursorExpired, "a read member at another branch")
}

// The facade entry is charged AS ITS SUBS GROW (review L3), not only when the
// call ends: during an export over a cold cache, the accounted size the
// cache holds for the facade, read before each unit is planned, is positive
// from the first unit (the subs' tables) and grows. An export or far seek holds
// the entry for its whole run, so the budget must see it grow.
//
// Sabotage (drop the parent redirect in fsCacheAccount): a sub's growth is
// charged to nothing, the facade reads 0 throughout.
func TestLensFactSet_FacadeIsChargedWhileItGrows(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	req := lt.request("anything", "kb/u/")
	req.Options.Depth = 1
	p := lt.first(t, req)
	DropFactSetCacheForTest()
	var mu sync.Mutex
	var sizes []int64
	defer SetFactSetExtendHookForTest(func(int) {
		_, n := FactSetCachedForTest(*p.Cursor)
		mu.Lock()
		sizes = append(sizes, n)
		mu.Unlock()
	})()
	require.NoError(t, FactSetExport(context.Background(), lt.scope(), lt.lens, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return io.Discard }))
	require.Len(t, sizes, 6, "one extension per unit")
	require.Positive(t, sizes[0], "the subs' tables are charged to the facade before unit 1 is planned")
	require.Greater(t, sizes[len(sizes)-1], sizes[0], "the facade's charge grows with its subs' prefixes")
}
