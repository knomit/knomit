package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrUnknownFamily: no family has that id.
var ErrUnknownFamily = errors.New("oauth: no such approved login")

// Login is one approved login as the operator's list shows it: a family
// that can still be used, and when it last minted a token.
type Login struct {
	Family
	// LastUsedAt is when the family's newest token was minted — the code
	// exchange, then every refresh. DERIVED from oauth_tokens, not written per
	// request: a client that keeps using one access token shows the time it
	// got that token, at most the access TTL stale. Zero: no token yet (the
	// code was never exchanged).
	LastUsedAt time.Time
}

// ListLogins returns the families that can still be used — not revoked and
// short of their absolute refresh end — newest first. A revoked or lapsed
// family is not listed: nothing can be done with it, and the client must ask
// again either way.
func (s *Store) ListLogins(ctx context.Context) ([]Login, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+familyCols+`, (SELECT MAX(t.created_at) FROM oauth_tokens t WHERE t.family = f.id)
FROM oauth_families f
WHERE f.revoked_at IS NULL AND f.refresh_expires_at > ?
ORDER BY f.created_at DESC, f.id`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Login
	for rows.Next() {
		var last sql.NullInt64
		f, err := scanFamily(rows.Scan, &last)
		if err != nil {
			return nil, err
		}
		l := Login{Family: f}
		if last.Valid {
			l.LastUsedAt = time.Unix(last.Int64, 0)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RevokeFamily revokes the family id: every token and code in it is dead from
// this moment, by the same revoked_at that a replayed refresh token or code
// sets. Revoking a family already revoked is not an error. An unknown id is
// ErrUnknownFamily.
func (s *Store) RevokeFamily(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := familyTx(ctx, tx, id); err != nil {
		if errors.Is(err, ErrUnknownToken) {
			return fmt.Errorf("%w: %q", ErrUnknownFamily, id)
		}
		return err
	}
	if err := s.revokeFamilyTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}
