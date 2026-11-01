package materialize

import (
	"testing"
	"time"
)

// Test seams of the FactSet engine. Each panics outside a test binary; the
// archtest keeps every production file from naming one.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("materialize." + name + " called outside a test binary")
	}
}

// SetFactSetExtendHookForTest runs f before the call holding a FactSet's
// extension token plans each unit past the planned prefix (a test blocks an
// extension in it), and returns the restore.
func SetFactSetExtendHookForTest(f func(unit int)) (restore func()) {
	mustBeTest("SetFactSetExtendHookForTest")
	old := factsetExtendHook
	factsetExtendHook = f
	return func() { factsetExtendHook = old }
}

// SetFactSetBudgetWaitForTest sets how long a call waits for planning memory
// before ErrFactSetBusy, and returns the restore.
func SetFactSetBudgetWaitForTest(d time.Duration) (restore func()) {
	mustBeTest("SetFactSetBudgetWaitForTest")
	old := factsetBudgetWait.Load()
	factsetBudgetWait.Store(int64(d))
	return func() { factsetBudgetWait.Store(old) }
}

// HoldFactSetPlanningMemoryForTest reserves n bytes of planning memory as a
// plan in flight would (at once, whatever the budget) and returns its
// release: with the budget below n, every call that must plan waits.
func HoldFactSetPlanningMemoryForTest(n int64) (release func()) {
	mustBeTest("HoldFactSetPlanningMemoryForTest")
	fsCache.mu.Lock()
	fsCache.total += n
	fsCache.inflight++
	fsCache.mu.Unlock()
	r := &fsReservation{size: n}
	return r.release
}

// FactSetPlansInFlightForTest is how many calls hold planning memory now.
func FactSetPlansInFlightForTest() int {
	mustBeTest("FactSetPlansInFlightForTest")
	fsCache.mu.Lock()
	defer fsCache.mu.Unlock()
	return fsCache.inflight
}

// SetFactSetExportFaultForTest makes the export fail before planning unit u
// (any unit after the first chunk was written) with f's error, and returns
// the restore.
func SetFactSetExportFaultForTest(f func(unit int) error) (restore func()) {
	mustBeTest("SetFactSetExportFaultForTest")
	old := factsetExportFault
	factsetExportFault = f
	return func() { factsetExportFault = old }
}

// SetFactSetSearchHookForTest runs f between a first call's search and its
// second watermark read, and returns the restore.
func SetFactSetSearchHookForTest(f func()) (restore func()) {
	mustBeTest("SetFactSetSearchHookForTest")
	old := factsetSearchHook
	factsetSearchHook = f
	return func() { factsetSearchHook = old }
}

// SetFactSetClockForTest replaces the heartbeat's clock, and returns the
// restore.
func SetFactSetClockForTest(now func() time.Time) (restore func()) {
	mustBeTest("SetFactSetClockForTest")
	old := factsetNow
	factsetNow = now
	return func() { factsetNow = old }
}

// SetFactHashForTest replaces FactHash (the identity, not the check hash),
// so a test can force two facts onto one hash, and returns the restore.
func SetFactHashForTest(f func(repo, rel string) uint64) (restore func()) {
	mustBeTest("SetFactHashForTest")
	old := factHash
	factHash = f
	return func() { factHash = old }
}

// DropFactSetCacheForTest empties the FactSet cache: the next call on any
// FactSet rebuilds it from its frozen row, planning again what it needs.
func DropFactSetCacheForTest() {
	mustBeTest("DropFactSetCacheForTest")
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.byID {
		c.removeLocked(e)
	}
}

// FactSetCachedForTest reports whether the FactSet with this cursor (or id)
// is in the cache, and its accounted bytes.
func FactSetCachedForTest(cursor string) (bool, int64) {
	mustBeTest("FactSetCachedForTest")
	id, _, ok := parseCursor(cursor)
	if !ok {
		id = cursor
	}
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byID[id]
	if !ok {
		return false, 0
	}
	return true, e.size
}

// SetFactSetPrefetchWindowForTest sets how many units are read ahead at once
// (at least 1), and returns the restore.
func SetFactSetPrefetchWindowForTest(n int) (restore func()) {
	mustBeTest("SetFactSetPrefetchWindowForTest")
	old := prefetchWindow
	prefetchWindow = max(n, 1)
	return func() { prefetchWindow = old }
}

// FactSetBackgroundReadsForTest is how many read-ahead windows have been
// started in the background so far.
func FactSetBackgroundReadsForTest() int64 {
	mustBeTest("FactSetBackgroundReadsForTest")
	return backgroundReads.Load()
}

// SetFactSetAnchorWaitForTest sets session.factset_anchor_wait and returns
// the restore.
func SetFactSetAnchorWaitForTest(d time.Duration) (restore func()) {
	mustBeTest("SetFactSetAnchorWaitForTest")
	old := factsetAnchorWait.Load()
	factsetAnchorWait.Store(int64(d))
	return func() { factsetAnchorWait.Store(old) }
}
