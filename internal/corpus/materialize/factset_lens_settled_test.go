package materialize

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

// wipeIndexMarkers makes f's agent/test branch read as a store whose index
// was never built (a new member, or a wiped index DB): no watermark, no
// settled-once marker, no rebuild generation, no pending sync.
func wipeIndexMarkers(t *testing.T, f *factsetFixture) {
	t.Helper()
	_, err := storetest.RawDB(f.svc).Exec(`DELETE FROM meta WHERE key IN (?, ?, ?, ?)`,
		"last_commit:agent/test", index.SettledOnceKey("agent/test"), index.RebuildGenKey("agent/test"), index.SyncPendingKey("agent/test"))
	require.NoError(t, err)
}

// A member whose FIRST build is an Index.Rebuild (the way a new member is
// built) has never had a settled watermark while that Rebuild runs, although
// the Rebuild has already bumped the generation (watermark "", generation
// "1"): it is EMPTY, and the lens FactSet answers without it. If that initial
// Rebuild FAILS, the member stays never-settled: still empty, never
// permanently busy. Once a Rebuild completes, the member is settled.
//
// Sabotage (the old predicate, watermark "" AND generation ""): mid-initial-
// Rebuild gamma reads ("", "1"), is waited on, and the call is busy.
func TestLensFactSet_InitialRebuildMemberIsEmpty(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	defer SetFactSetAnchorWaitForTest(150 * time.Millisecond)()
	ctx := context.Background()
	gammaRoots := func(p FactSetPage) (n int) {
		for _, r := range unitRoots(allPages(t, lt.scope(), lt.lens, p)...) {
			if r.Repo == lt.r2.id12() {
				n++
			}
		}
		return n
	}

	// In flight.
	wipeIndexMarkers(t, lt.r2)
	var (
		once          sync.Once
		w, gen        string
		settledOnce   bool
		page          FactSetPage
		callErr, rErr error
	)
	require.NoError(t, lt.r2.svc.IndexManager().Rebuild(ctx, "agent/test", func(string, int, int) {
		once.Do(func() {
			w, gen, _, rErr = lt.r2.svc.HistoryQuery().SettledWatermark(ctx, "agent/test")
			if rErr == nil {
				settledOnce, rErr = lt.r2.svc.HistoryQuery().SettledOnce(ctx, "agent/test")
			}
			page, _, callErr = FactSetFirstPage(ctx, lt.scope(), lt.request("anything", "kb/u/"))
		})
	}))
	require.NoError(t, rErr)
	require.Equal(t, [2]string{"", "1"}, [2]string{w, gen}, "an initial Rebuild has bumped the generation and has no watermark yet")
	require.False(t, settledOnce, "never settled")
	require.NoError(t, callErr, "the member being built for the first time is empty, not waited on")
	require.Equal(t, 4, page.Units, "alpha's and beta's results only")
	require.Zero(t, gammaRoots(page))
	require.NotContains(t, page.Anchors, lt.r2.id12())
	done, err := lt.r2.svc.HistoryQuery().SettledOnce(ctx, "agent/test")
	require.NoError(t, err)
	require.True(t, done, "a completed Rebuild settles it")

	// Failed: the initial Rebuild is cancelled midway.
	wipeIndexMarkers(t, lt.r2)
	rctx, cancel := context.WithCancel(ctx)
	require.Error(t, lt.r2.svc.IndexManager().Rebuild(rctx, "agent/test", func(string, int, int) { cancel() }))
	cancel()
	w, gen, _, err = lt.r2.svc.HistoryQuery().SettledWatermark(ctx, "agent/test")
	require.NoError(t, err)
	require.Empty(t, w)
	require.NotEmpty(t, gen, "the failed Rebuild left its generation behind")
	p, _, err := FactSetFirstPage(ctx, lt.scope(), lt.request("anything", "kb/u/"))
	require.NoError(t, err, "a member whose initial build failed is empty, not permanently busy")
	require.Equal(t, 4, p.Units)
	require.Zero(t, gammaRoots(p))
	require.NotContains(t, p.Anchors, lt.r2.id12())
}

// The settled-once marker's lifecycle in the store: a store indexed BEFORE the
// marker existed (a watermark, no marker) reads settled, and a Rebuild of it
// keeps it settled while the watermark is cleared (the marker is backfilled
// first); a branch created from a settled one inherits it; dropping the
// branch removes it.
//
// Sabotage (drop the Rebuild backfill): mid-Rebuild the legacy store reads
// never-settled.
func TestSettledOnce_LegacyStoreRecreatedAndDroppedBranches(t *testing.T) {
	f := newLensMember(t, "alpha")
	ctx := context.Background()
	hq := f.svc.HistoryQuery()
	_, err := storetest.RawDB(f.svc).Exec(`DELETE FROM meta WHERE key = ?`, index.SettledOnceKey("agent/test"))
	require.NoError(t, err)
	settled, err := hq.SettledOnce(ctx, "agent/test")
	require.NoError(t, err)
	require.True(t, settled, "a legacy store with a watermark and no marker is settled")
	var mid bool
	var midErr error
	var o sync.Once
	require.NoError(t, f.svc.IndexManager().Rebuild(ctx, "agent/test", func(string, int, int) {
		o.Do(func() { mid, midErr = hq.SettledOnce(ctx, "agent/test") })
	}))
	require.NoError(t, midErr)
	require.True(t, mid, "a legacy store stays settled while it rebuilds")

	require.NoError(t, f.svc.CreateBranch(ctx, "agent/child", "agent/test"))
	settled, err = hq.SettledOnce(ctx, "agent/child")
	require.NoError(t, err)
	require.True(t, settled, "a created branch inherits the settled state")
	require.NoError(t, storetest.DropBranch(ctx, f.svc, "agent/child"))
	settled, err = hq.SettledOnce(ctx, "agent/child")
	require.NoError(t, err)
	require.False(t, settled, "a dropped branch is not settled")
}
