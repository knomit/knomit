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
	// code is still waiting to be exchanged).
	LastUsedAt time.Time
}

// ListLogins returns the families that can still be used, newest first: not
// revoked, short of their absolute refresh end, and holding a way in — a
// token already issued, or a code that is still live and unspent. A family
// whose code was never exchanged, or was spent by an exchange that then
// failed (a wrong PKCE verifier), can never yield a token and is not listed.
// Neither is a revoked or lapsed one: the client must ask again either way.
//
// The client name is read here only, under an alias no other query uses: the
// per-request token lookups (familyCols) never pay for it.
func (s *Store) ListLogins(ctx context.Context) ([]Login, error) {
	now := s.now().Unix()
	rows, err := s.db.QueryContext(ctx, `
SELECT `+familyCols+`,
  COALESCE((SELECT fcl.client_name FROM oauth_family_clients fcl WHERE fcl.family_id = f.id), ''),
  (SELECT MAX(t.created_at) FROM oauth_tokens t WHERE t.family = f.id)
FROM oauth_families f
WHERE f.revoked_at IS NULL AND f.refresh_expires_at > ?
  AND (EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.family = f.id)
    OR EXISTS (SELECT 1 FROM oauth_codes oc WHERE oc.family = f.id AND oc.consumed_at IS NULL AND oc.expires_at > ?))
ORDER BY f.created_at DESC, f.id`, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Login
	for rows.Next() {
		var (
			name string
			last sql.NullInt64
		)
		f, err := scanFamily(rows.Scan, &name, &last)
		if err != nil {
			return nil, err
		}
		f.ClientName = name
		l := Login{Family: f}
		if last.Valid {
			l.LastUsedAt = time.Unix(last.Int64, 0)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Revocation is what RevokeFamily left: when the family was revoked and who
// revoked it. RevokedBy is "" when the server revoked it itself (a replay, an
// approval that no longer stands) or before 000021 recorded it.
type Revocation struct {
	FamilyID  string
	RevokedAt time.Time
	RevokedBy string
}

// RevokeFamily revokes the family id on behalf of by: every token and code in
// it is dead from this moment, by the same revoked_at that a replayed refresh
// token or code sets, and by is recorded in oauth_family_revocations in the
// same transaction. Revoking a family already revoked is not an error and
// changes nothing: the first revocation, and its revoker, stand. An unknown id
// is ErrUnknownFamily.
func (s *Store) RevokeFamily(ctx context.Context, id, by string) (Revocation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Revocation{}, err
	}
	defer tx.Rollback()
	f, err := familyTx(ctx, tx, id)
	if errors.Is(err, ErrUnknownToken) {
		return Revocation{}, fmt.Errorf("%w: %q", ErrUnknownFamily, id)
	}
	if err != nil {
		return Revocation{}, err
	}
	if !f.Revoked {
		if err := s.revokeFamilyTx(ctx, tx, id); err != nil {
			return Revocation{}, err
		}
		if by != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO oauth_family_revocations (family_id, revoked_by) VALUES (?, ?)`, id, by); err != nil {
				return Revocation{}, err
			}
		}
	}
	var (
		r  = Revocation{FamilyID: id}
		at int64
	)
	if err := tx.QueryRowContext(ctx, `
SELECT f.revoked_at, COALESCE((SELECT fr.revoked_by FROM oauth_family_revocations fr WHERE fr.family_id = f.id), '')
FROM oauth_families f WHERE f.id = ?`, id).Scan(&at, &r.RevokedBy); err != nil {
		return Revocation{}, err
	}
	r.RevokedAt = time.Unix(at, 0)
	return r, tx.Commit()
}
