// Package oauth makes a knomit instance an OAuth 2.1 authorization server and
// resource server for exactly one resource, itself (F19 phase 3a). It issues
// opaque bearer tokens by the authorization-code flow with PKCE, after an
// operator approves the request locally, and verifies them on the OAuth
// listener — the only listener where a bearer token is ever judged.
//
// Nothing here decides what a principal may do. A verified token becomes an
// auth.Principal{Kind: host, Via: token} plus a ceiling, and auth.Allowed
// answers the rest, as it does for every other principal.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Named refusals. The verifier maps every one of them to the same 401
// invalid_token; they are distinct so tests and logs can say WHICH.
var (
	ErrUnknownToken  = errors.New("oauth: unknown token")
	ErrExpired       = errors.New("oauth: token expired")
	ErrRevoked       = errors.New("oauth: token revoked")
	ErrReused        = errors.New("oauth: rotated refresh token presented again; family revoked")
	ErrWrongAudience = errors.New("oauth: token not issued for this resource")
)

const (
	kindAccess  = "access"
	kindRefresh = "refresh"
)

// Store is the control.db side of the issuer: token families, tokens,
// codes and pending authorizations. It borrows the control.db handle, which
// production opens with ONE connection — so every statement inside a
// transaction goes through the tx, never through db, or it deadlocks.
type Store struct {
	db         *sql.DB
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

func NewStore(db *sql.DB, accessTTL, refreshTTL time.Duration) *Store {
	return &Store{db: db, accessTTL: accessTTL, refreshTTL: refreshTTL, now: time.Now}
}

// FamilySpec is what an approved, collected authorization turns into: one
// client, one subject, one ceiling, one audience.
type FamilySpec struct {
	ClientID string
	Subject  string
	Scopes   []string
	Resource string
}

// Family is one login. It is the unit of revocation.
type Family struct {
	FamilySpec
	ID               string
	CreatedAt        time.Time
	RefreshExpiresAt time.Time
	Revoked          bool
}

// Issued is a freshly minted access+refresh pair. The two strings exist only
// here and in the response; the store keeps their hashes.
type Issued struct {
	Access          string
	Refresh         string
	AccessExpiresAt time.Time
	Family          Family
}

// newSecret is 32 random bytes, base64url without padding.
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// hashSecret is what the store keys on. A lookup by the SHA-256 of a
// 256-bit random secret is not a timing oracle an attacker can use: learning
// a hash prefix does not help find a preimage.
func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// CreateFamily starts a login. Its refresh lifetime is fixed here, from now.
func (s *Store) CreateFamily(ctx context.Context, spec FamilySpec) (Family, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Family{}, err
	}
	defer tx.Rollback()
	f, err := s.createFamilyTx(ctx, tx, spec)
	if err != nil {
		return Family{}, err
	}
	return f, tx.Commit()
}

func (s *Store) createFamilyTx(ctx context.Context, tx *sql.Tx, spec FamilySpec) (Family, error) {
	id, err := newSecret()
	if err != nil {
		return Family{}, err
	}
	now := s.now()
	f := Family{FamilySpec: spec, ID: id, CreatedAt: now, RefreshExpiresAt: now.Add(s.refreshTTL)}
	_, err = tx.ExecContext(ctx, `
INSERT INTO oauth_families (id, client_id, subject, scope, resource, created_at, refresh_expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.ID, spec.ClientID, spec.Subject, strings.Join(spec.Scopes, " "), spec.Resource,
		now.Unix(), f.RefreshExpiresAt.Unix())
	return f, err
}

// IssuePair mints an access+refresh pair in family.
func (s *Store) IssuePair(ctx context.Context, family string) (Issued, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Issued{}, err
	}
	defer tx.Rollback()
	f, err := familyTx(ctx, tx, family)
	if err != nil {
		return Issued{}, err
	}
	if f.Revoked {
		return Issued{}, ErrRevoked
	}
	out, err := s.issuePairTx(ctx, tx, f)
	if err != nil {
		return Issued{}, err
	}
	return out, tx.Commit()
}

func (s *Store) issuePairTx(ctx context.Context, tx *sql.Tx, f Family) (Issued, error) {
	now := s.now()
	if !now.Before(f.RefreshExpiresAt) {
		return Issued{}, ErrExpired
	}
	access, err := newSecret()
	if err != nil {
		return Issued{}, err
	}
	refresh, err := newSecret()
	if err != nil {
		return Issued{}, err
	}
	// Nothing outlives its family: an access token minted near the end of a
	// login is cut short rather than extending it.
	accessExp := now.Add(s.accessTTL)
	if accessExp.After(f.RefreshExpiresAt) {
		accessExp = f.RefreshExpiresAt
	}
	const ins = `INSERT INTO oauth_tokens (hash, kind, family, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, ins, hashSecret(access), kindAccess, f.ID, now.Unix(), accessExp.Unix()); err != nil {
		return Issued{}, err
	}
	if _, err := tx.ExecContext(ctx, ins, hashSecret(refresh), kindRefresh, f.ID, now.Unix(), f.RefreshExpiresAt.Unix()); err != nil {
		return Issued{}, err
	}
	return Issued{Access: access, Refresh: refresh, AccessExpiresAt: accessExp, Family: f}, nil
}

// tokenRow is one oauth_tokens row joined to its family.
type tokenRow struct {
	kind      string
	expiresAt time.Time
	rotated   bool
	family    Family
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

const familyCols = `f.id, f.client_id, f.subject, f.scope, f.resource, f.created_at, f.refresh_expires_at, f.revoked_at`

func scanFamily(scan func(...any) error, extra ...any) (Family, error) {
	var (
		f                  Family
		scope              string
		created, refreshTo int64
		revoked            sql.NullInt64
	)
	dest := append([]any{&f.ID, &f.ClientID, &f.Subject, &scope, &f.Resource, &created, &refreshTo, &revoked}, extra...)
	if err := scan(dest...); err != nil {
		return Family{}, err
	}
	f.Scopes = strings.Fields(scope)
	f.CreatedAt, f.RefreshExpiresAt = time.Unix(created, 0), time.Unix(refreshTo, 0)
	f.Revoked = revoked.Valid
	return f, nil
}

func familyTx(ctx context.Context, q querier, id string) (Family, error) {
	f, err := scanFamily(q.QueryRowContext(ctx, `SELECT `+familyCols+` FROM oauth_families f WHERE f.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Family{}, fmt.Errorf("oauth: family %q: %w", id, ErrUnknownToken)
	}
	return f, err
}

func lookupToken(ctx context.Context, q querier, token string) (tokenRow, error) {
	var (
		r       tokenRow
		exp     int64
		rotated sql.NullInt64
	)
	f, err := scanFamily(q.QueryRowContext(ctx, `
SELECT `+familyCols+`, t.kind, t.expires_at, t.rotated_at
FROM oauth_tokens t JOIN oauth_families f ON f.id = t.family
WHERE t.hash = ?`, hashSecret(token)).Scan, &r.kind, &exp, &rotated)
	if errors.Is(err, sql.ErrNoRows) {
		return tokenRow{}, ErrUnknownToken
	}
	if err != nil {
		return tokenRow{}, err
	}
	r.family, r.expiresAt, r.rotated = f, time.Unix(exp, 0), rotated.Valid
	return r, nil
}

// LookupAccess answers the family an access token belongs to, or why it
// cannot be used: unknown (including a refresh token presented as a bearer),
// revoked, or expired.
func (s *Store) LookupAccess(ctx context.Context, token string) (Family, error) {
	r, err := lookupToken(ctx, s.db, token)
	if err != nil {
		return Family{}, err
	}
	if r.kind != kindAccess {
		return Family{}, ErrUnknownToken
	}
	if r.family.Revoked {
		return Family{}, ErrRevoked
	}
	if !s.now().Before(r.expiresAt) {
		return Family{}, ErrExpired
	}
	return r.family, nil
}

// Refresh exchanges a refresh token for a new pair in ONE transaction: the
// presented token is marked rotated and the new pair inserted together, so
// two racing exchanges cannot both win. Presenting a token that was already
// rotated means two parties hold it; the whole family is revoked (committed,
// not rolled back) and ErrReused returned.
//
// clientID must be the family's client (ErrWrongClient), and resource, when
// not empty, the family's resource exactly (ErrWrongAudience); an empty
// resource reuses the family's, because a refresh may omit it. Both are
// checked BEFORE rotating, so a refused attempt does not cost the real owner
// their token. So is scopes: a refresh may name a subset of the family's
// ceiling (the response still carries the family's, which is what the new
// token holds) but never anything beyond it (ErrInvalidScope).
func (s *Store) Refresh(ctx context.Context, refresh, clientID, resource string, scopes []string) (Issued, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Issued{}, err
	}
	defer tx.Rollback()
	r, err := lookupToken(ctx, tx, refresh)
	if err != nil {
		return Issued{}, err
	}
	if r.kind != kindRefresh {
		return Issued{}, ErrUnknownToken
	}
	if r.family.Revoked {
		return Issued{}, ErrRevoked
	}
	if r.family.ClientID != clientID {
		return Issued{}, ErrWrongClient
	}
	if resource != "" && resource != r.family.Resource {
		return Issued{}, ErrWrongAudience
	}
	for _, sc := range scopes {
		if !slices.Contains(r.family.Scopes, sc) {
			return Issued{}, ErrInvalidScope
		}
	}
	reuse := func() (Issued, error) {
		if err := s.revokeFamilyTx(ctx, tx, r.family.ID); err != nil {
			return Issued{}, err
		}
		if err := tx.Commit(); err != nil {
			return Issued{}, err
		}
		return Issued{}, ErrReused
	}
	if r.rotated {
		return reuse()
	}
	// The family's absolute end is checked once, in issuePairTx: past it,
	// the error rolls this transaction back, rotation included.
	// No second "was it still unrotated?" check on this UPDATE: the
	// read above and this write share one transaction on control.db's single
	// connection, so nothing can rotate the token in between.
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET rotated_at = ? WHERE hash = ?`,
		s.now().Unix(), hashSecret(refresh)); err != nil {
		return Issued{}, err
	}
	out, err := s.issuePairTx(ctx, tx, r.family)
	if err != nil {
		return Issued{}, err
	}
	return out, tx.Commit()
}

// Revoke revokes the family of token, whichever kind it is (RFC 7009 lets
// the server revoke related tokens, and a family is one login). An unknown
// token is not an error: the revocation endpoint answers 200 either way.
func (s *Store) Revoke(ctx context.Context, token string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := lookupToken(ctx, tx, token)
	if errors.Is(err, ErrUnknownToken) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.revokeFamilyTx(ctx, tx, r.family.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) revokeFamilyTx(ctx context.Context, tx *sql.Tx, family string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE oauth_families SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		s.now().Unix(), family)
	return err
}
