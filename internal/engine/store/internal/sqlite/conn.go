package sqlite

import (
	"context"
	"database/sql"

	storegit "knomit/internal/engine/store/git"
)

// Conn returns the executor for ctx: the *sql.Tx a caller threaded through ctx
// when there is one, else db. Every store statement goes through it so a
// ctx-carried transaction participates in the git storer and the index alike.
func Conn(ctx context.Context, db *sql.DB) storegit.CtxExecer {
	return storegit.Conn(ctx, db)
}

// BeginTxIfNeeded opens a transaction unless ctx already carries one. The
// bool reports whether this call opened it, and so must commit or roll it back.
func BeginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	return storegit.BeginTxIfNeeded(ctx, db)
}
