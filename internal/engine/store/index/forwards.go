package index

import (
	"context"
	"database/sql"

	"knomit/internal/engine/store/model"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/sqlite"
)

// The unexported names below are the ones the index files used when they
// lived in package store, where sqlite_conn.go supplied them as one-line
// forwards (the parent keeps its own for the files that stayed). They are
// kept here under the same names so the moved files did not change; each is
// the sqlite child's definition, not a copy of it. textnorm_alias.go moved
// here whole: after the move nothing left in package store calls it.

func conn(ctx context.Context, db *sql.DB) storegit.CtxExecer {
	return sqlite.Conn(ctx, db)
}

func beginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	return sqlite.BeginTxIfNeeded(ctx, db)
}

var (
	float32SliceToBytes  = sqlite.Float32SliceToBytes
	bytesToFloat32Slice  = sqlite.BytesToFloat32Slice
	usableKNNSimilarity  = sqlite.UsableKNNSimilarity
	groupingKey          = sqlite.GroupingKey
	motifClusterKeyExpr  = sqlite.MotifClusterKeyExpr
	precomputedEmbedding = model.PrecomputedEmbedding
)
