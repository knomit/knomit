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

// ReserveFactSetBuildsForTest takes n planning slots and returns their
// release, so a test drives the cap without timing.
func ReserveFactSetBuildsForTest(n int) func() {
	mustBeTest("ReserveFactSetBuildsForTest")
	for range n {
		buildSlots <- struct{}{}
	}
	return func() {
		for range n {
			<-buildSlots
		}
	}
}

// ReserveFactSetExportsForTest takes n export slots and returns their release.
func ReserveFactSetExportsForTest(n int) func() {
	mustBeTest("ReserveFactSetExportsForTest")
	for range n {
		exportSlots <- struct{}{}
	}
	return func() {
		for range n {
			<-exportSlots
		}
	}
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
// FactSet rebuilds it from its frozen row by replay.
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
