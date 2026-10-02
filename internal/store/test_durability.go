package store

import (
	"sync/atomic"
	"testing"
)

// testFastDurability is the switch SetTestFastDurability flips. It is read
// only inside a test binary; see fastDurability.
var testFastDurability atomic.Bool

// SetTestFastDurability turns SQLite's fsyncs off (synchronous=OFF) for every
// database this test binary opens from then on: the main store DB (the
// ConnectHook in vec.go), the session sidecar (already OFF, see
// openSessionDB) and control.db (repos.OpenRegistryNoSchema,
// repos.OpenLensRegistry, and test fixtures that splice SyncDSNParam into
// their own DSN). Call it from a test package's TestMain, before m.Run, so no
// connection opened before the switch keeps the slow setting. Pass false to
// restore the production setting.
//
// Why it exists (#365): on Windows a repo create costs ~0.8s, almost all of it
// WAL file creation and the fsyncs of checkpoint and close, and packages such
// as internal/web create hundreds of repos. A test that loses power mid-run has
// lost nothing worth keeping, so those fsyncs buy a test binary nothing.
//
// What it deliberately does NOT change: the journal mode. Every DB stays in
// WAL, because WAL decides who blocks whom — readers proceed alongside a
// writer — and concurrency tests (TestPathHistory_RebuildDoesNotBlockWriters,
// #366) assert exactly that. synchronous only decides when bytes reach the
// platter, which no test in a running process can observe. A test of crash
// recovery or fsync ordering CAN observe it and must not run under the switch:
// it calls SetTestFastDurability(false) for its own duration (and must not be
// t.Parallel while it does, the switch being process-wide).
//
// It panics outside a test binary (testing.Testing), and fastDurability
// ignores it there too, so a production knomit always runs at the durability
// its DSNs and pragmas name. test/archtest keeps production source from even
// naming it.
func SetTestFastDurability(on bool) {
	if !testing.Testing() {
		panic("store.SetTestFastDurability called outside a test binary")
	}
	testFastDurability.Store(on)
}

// fastDurability reports whether SetTestFastDurability is on. Always false
// outside a test binary.
func fastDurability() bool {
	return testing.Testing() && testFastDurability.Load()
}

// SyncDSNParam returns the DSN parameter a go-sqlite3 ("sqlite3" driver) DSN
// appends to honour SetTestFastDurability: "&_synchronous=OFF" while the test
// switch is on, "" otherwise — so in production the DSN, and with it the
// durability, is exactly what the caller wrote. It assumes the DSN already has
// a "?" query. It is for DBs opened outside this package, chiefly control.db;
// the main store DB sets synchronous in its ConnectHook instead, which runs
// AFTER go-sqlite3 applies the DSN and would override a DSN value.
func SyncDSNParam() string {
	if fastDurability() {
		return "&_synchronous=OFF"
	}
	return ""
}
