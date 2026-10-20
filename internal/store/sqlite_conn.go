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
// the sub-indices read as before, and re-exports the few names callers outside
// store use (Crypt, CosineSim, SetTestFastDurability, SyncDSNParam,
// WithPrecomputedEmbeddings) so the public surface is unchanged.

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

// Crypt provides AES-256-GCM encryption for credential storage (see the
// sqlite child).
type Crypt = sqlite.Crypt

// NewCrypt derives a Crypt from SSH private key material.
func NewCrypt(keyMaterial []byte) (*Crypt, error) { return sqlite.NewCrypt(keyMaterial) }

// CosineSim is the cosine similarity of two vectors, 0 when undefined.
func CosineSim(a, b []float32) float64 { return sqlite.CosineSim(a, b) }

// WithPrecomputedEmbeddings returns a context carrying a path → vector lookup
// the indexer consults before invoking the embedder. See the sqlite child for
// the trust contract on donated vectors.
func WithPrecomputedEmbeddings(ctx context.Context, byPath map[string][]float32) context.Context {
	return sqlite.WithPrecomputedEmbeddings(ctx, byPath)
}

// SetTestFastDurability turns SQLite's fsyncs off for every database this test
// binary opens from then on. Test binaries only; it panics elsewhere.
func SetTestFastDurability(on bool) { sqlite.SetTestFastDurability(on) }

// SyncDSNParam is the DSN suffix that honours SetTestFastDurability ("" in
// production).
func SyncDSNParam() string { return sqlite.SyncDSNParam() }
