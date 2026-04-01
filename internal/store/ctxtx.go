package store

import (
	"context"
	"database/sql"

	storegit "knomit/internal/store/git"
)

func conn(ctx context.Context, db *sql.DB) storegit.CtxExecer {
	return storegit.Conn(ctx, db)
}

func beginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	return storegit.BeginTxIfNeeded(ctx, db)
}

// WithTx and TxFromContext are re-exported for external callers (synthesize, etc.)
var WithTx = storegit.WithTx
var TxFromContext = storegit.TxFromContext
