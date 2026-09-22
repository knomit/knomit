package auth

import (
	"context"
	"database/sql"
	"time"
)

// SQLGrants is the control.db implementation of Grants. It borrows the
// control.db handle exactly as internal/client/sessions.Store does rather
// than owning it; every method is one statement.
type SQLGrants struct{ db *sql.DB }

func NewSQLGrants(db *sql.DB) *SQLGrants { return &SQLGrants{db: db} }

func (g *SQLGrants) For(ctx context.Context, p Principal) (Set, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT permission FROM grants WHERE principal = ? AND revoked_at IS NULL`, p.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Set{}
	for rows.Next() {
		var perm string
		if err := rows.Scan(&perm); err != nil {
			return nil, err
		}
		out[Permission(perm)] = struct{}{}
	}
	return out, rows.Err()
}

// Grant inserts or revives a row. Idempotent: granting what is already live
// is a no-op, granting what was revoked clears revoked_at.
func (g *SQLGrants) Grant(ctx context.Context, p Principal, perm Permission, grantedBy string) error {
	_, err := g.db.ExecContext(ctx, `
INSERT INTO grants (principal, permission, granted_by, granted_at, revoked_at)
VALUES (?, ?, ?, ?, NULL)
ON CONFLICT(principal, permission) DO UPDATE SET
  granted_by = excluded.granted_by, granted_at = excluded.granted_at, revoked_at = NULL`,
		p.String(), string(perm), grantedBy, time.Now().Unix())
	return err
}

// Revoke stamps revoked_at; the row stays as the audit trail.
func (g *SQLGrants) Revoke(ctx context.Context, p Principal, perm Permission) error {
	_, err := g.db.ExecContext(ctx,
		`UPDATE grants SET revoked_at = ? WHERE principal = ? AND permission = ? AND revoked_at IS NULL`,
		time.Now().Unix(), p.String(), string(perm))
	return err
}

// EverGranted reports whether any row exists for principal+permission, live
// or revoked. Boot seeding uses it so a revocation survives a restart: a
// revoked row is history, not absence, and must never be revived by boot.
func (g *SQLGrants) EverGranted(ctx context.Context, p Principal, perm Permission) (bool, error) {
	var n int
	err := g.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM grants WHERE principal = ? AND permission = ?`, p.String(), string(perm)).Scan(&n)
	return n > 0, err
}
