package store

import (
	"context"
	"database/sql"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/sqlite"
)

// This file is the parent's seam to the sqlite child (internal/sqlite). The
// child owns the driver, vector codec, tx helpers and the other engine-level
// primitives; this package keeps its old unexported spellings as forwards so
// the sub-indices read as before. The one exported name left is SyncDSNParam,
// which reads sqlite package state; Crypt, CosineSim and
// WithPrecomputedEmbeddings moved to store/model.

func conn(ctx context.Context, db *sql.DB) storegit.CtxExecer {
	return sqlite.Conn(ctx, db)
}

func beginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	return sqlite.BeginTxIfNeeded(ctx, db)
}

var (
	registerVec         = sqlite.Register
	float32SliceToBytes = sqlite.Float32SliceToBytes
	groupingKey         = sqlite.GroupingKey
)

// SyncDSNParam is the DSN suffix that honours storetest.SetFastDurability ("" in
// production).
func SyncDSNParam() string { return sqlite.SyncDSNParam() }
