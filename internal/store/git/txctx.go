package git

import (
	"context"
	"database/sql"
)

// CtxExecer is the common interface between *sql.DB and *sql.Tx for context-aware operations.
type CtxExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type ctxKeyTx struct{}

// WithTx stores a *sql.Tx in the context for downstream methods.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, ctxKeyTx{}, tx)
}

// TxFromContext returns the *sql.Tx stored in context, or nil.
func TxFromContext(ctx context.Context) *sql.Tx {
	tx, _ := ctx.Value(ctxKeyTx{}).(*sql.Tx)
	return tx
}

// Conn returns the tx from context if present, otherwise db.
func Conn(ctx context.Context, db *sql.DB) CtxExecer {
	if tx := TxFromContext(ctx); tx != nil {
		return tx
	}
	return db
}

// WithoutTx returns a context with any stored *sql.Tx removed. Use it after
// committing or rolling back a tx obtained from BeginTxIfNeeded, before
// calling code that should run against the bare *sql.DB rather than the
// now-closed tx.
func WithoutTx(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyTx{}, nil)
}

// BeginTxIfNeeded starts a new transaction if ctx doesn't already carry one.
// Returns the enriched context, the tx, whether this call owns the tx
// (caller must commit/rollback), and any error. If ctx already has a tx,
// returns it with ownTx=false.
func BeginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	if tx := TxFromContext(ctx); tx != nil {
		return ctx, tx, false, nil
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return ctx, nil, false, err
	}
	return WithTx(ctx, tx), tx, true, nil
}
