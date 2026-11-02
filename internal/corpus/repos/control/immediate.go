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
// (rolls back otherwise). IMMEDIATE takes control.db's write lock before the
// first read, so a read-check-write (a secret's host list against its linked
// repos, a link against the secret's host list) cannot interleave with
// another writer's: the two serialise, and the second one reads what the
// first committed. Every write that adds a secret link, and every change to
// a secret's host list, runs in one of these.
//
// fn must use only tx: a call on the *sql.DB inside fn waits for the
// connection this transaction holds.
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
