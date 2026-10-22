package store

import "knomit/internal/engine/store/internal/sqlite"

// The sqlite child (store/internal/sqlite) owns the driver, vector codec, tx
// helpers and the other engine-level primitives; package store calls it
// directly. The one forward left is SyncDSNParam: sqlite cannot be imported
// from outside the store, and internal/corpus/repos and internal/edge/web open their
// control databases with its suffix. Crypt, CosineSim and
// WithPrecomputedEmbeddings live in store/model.

// SyncDSNParam is the DSN suffix that honours storetest.SetFastDurability ("" in
// production).
func SyncDSNParam() string { return sqlite.SyncDSNParam() }
