package control

import (
	"context"
	"database/sql"
	"fmt"
)

// dbTx is what the control.db write helpers need from a transaction: a
// *sql.Tx or an immediateConn.
type dbTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// immediateConn is a BEGIN IMMEDIATE transaction on a connection of its own,
// shaped like *sql.Tx for the helpers that take a dbTx.
type immediateConn struct {
	ctx context.Context
	c   *sql.Conn
}

func (t immediateConn) Exec(q string, args ...any) (sql.Result, error) {
	return t.c.ExecContext(t.ctx, q, args...)
}

func (t immediateConn) Query(q string, args ...any) (*sql.Rows, error) {
	return t.c.QueryContext(t.ctx, q, args...)
}

func (t immediateConn) QueryRow(q string, args ...any) *sql.Row {
	return t.c.QueryRowContext(t.ctx, q, args...)
}

// immediateTx runs fn inside BEGIN IMMEDIATE and commits when fn returns nil
// (rolls back otherwise). Every write that adds a secret link, and every
// change to a secret's host list, runs in one, with its read-check-write
// (a secret's host list against its linked repos, a link against the
// secret's host list) inside the same transaction.
//
// Within one process the registry's pool is already serialising: control.db
// is opened with SetMaxOpenConns(1) (OpenRegistryNoSchema), so two
// transactions never overlap. IMMEDIATE is defence in depth: it takes the
// write lock before the first read, so the check stays correct if the pool
// ever allows more connections or another process writes control.db.
//
// fn must use only tx: a call on the *sql.DB inside fn waits for the one
// connection this transaction holds, and deadlocks.
func immediateTx(db *sql.DB, op string, fn func(tx dbTx) error) error {
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s: conn: %w", op, err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := fn(immediateConn{ctx: ctx, c: c}); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true
	return nil
}
