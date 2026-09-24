package oauth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrUnknownPending = errors.New("oauth: no such authorization request")
	ErrNotPending     = errors.New("oauth: authorization request is not waiting for that")
	ErrCollected      = errors.New("oauth: authorization request already completed")
	ErrTooManyPending = errors.New("oauth: too many authorization requests are waiting")
	ErrWrongClient    = errors.New("oauth: token was issued to another client")
)

const (
	DecisionApproved = "approved"
	DecisionDenied   = "denied"

	// pendingTTL is how long a request waits for an operator, and codeTTL how
	// long a minted code may wait for its exchange. Both are the OAuth
	// norm for a code (RFC 6749 §4.1.2 recommends at most ten minutes).
	pendingTTL = 10 * time.Minute
	codeTTL    = 10 * time.Minute

	// maxLivePending bounds the requests waiting at once. /oauth/authorize is
	// unauthenticated on a listener a proxy exposes, and each request is a
	// row an operator has to read; a flood beyond this is refused with
	// temporarily_unavailable rather than written.
	maxLivePending = 100
)

// Pending is one parked authorization request, and what the operator sees
// before deciding: who asked (client, requester address and user agent),
// for what (scopes, resource), and where the answer goes (redirect URI).
type Pending struct {
	ID            string
	ClientID      string
	ClientName    string
	RedirectURI   string
	Scopes        []string // as requested
	CodeChallenge string
	Resource      string
	State         string
	RemoteAddr    string
	UserAgent     string
	CreatedAt     time.Time
	ExpiresAt     time.Time

	Decision  string   // "", DecisionApproved, DecisionDenied
	Subject   string   // set on approval
	Ceiling   []string // set on approval
	DecidedBy string
	Collected bool

	// GrantsUnchanged is set by Issuer.Approve only, never stored: the
	// subject had been granted before, so the approval wrote no grants and
	// the token is capped by the grants the operator left in place.
	GrantsUnchanged bool
}

// Expired reports whether an UNDECIDED request can no longer be decided.
func (p Pending) Expired(now time.Time) bool { return !now.Before(p.ExpiresAt) }

// CreatePending parks a validated request. Expired rows are purged first, so
// they neither count toward the cap nor accumulate.
func (s *Store) CreatePending(ctx context.Context, p Pending) (Pending, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending WHERE expires_at <= ?`, now.Unix()); err != nil {
		return Pending{}, err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_pending WHERE decision = ''`).Scan(&live); err != nil {
		return Pending{}, err
	}
	if live >= maxLivePending {
		return Pending{}, ErrTooManyPending
	}
	if p.ID, err = newSecret(); err != nil {
		return Pending{}, err
	}
	p.CreatedAt, p.ExpiresAt = now, now.Add(pendingTTL)
	p.Decision, p.Subject, p.Ceiling, p.DecidedBy, p.Collected = "", "", nil, "", false
	if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_pending (id, client_id, client_name, redirect_uri, scope, code_challenge, resource,
  state, remote_addr, user_agent, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ClientID, p.ClientName, p.RedirectURI, strings.Join(p.Scopes, " "), p.CodeChallenge, p.Resource,
		p.State, p.RemoteAddr, p.UserAgent, now.Unix(), p.ExpiresAt.Unix()); err != nil {
		return Pending{}, err
	}
	return p, tx.Commit()
}

const pendingCols = `id, client_id, client_name, redirect_uri, scope, code_challenge, resource, state,
  remote_addr, user_agent, created_at, expires_at, decision, subject, ceiling, decided_by, collected_at`

func scanPending(scan func(...any) error) (Pending, error) {
	var (
		p                Pending
		scope, ceiling   string
		created, expires int64
		collected        sql.NullInt64
	)
	if err := scan(&p.ID, &p.ClientID, &p.ClientName, &p.RedirectURI, &scope, &p.CodeChallenge, &p.Resource, &p.State,
		&p.RemoteAddr, &p.UserAgent, &created, &expires, &p.Decision, &p.Subject, &ceiling, &p.DecidedBy, &collected); err != nil {
		return Pending{}, err
	}
	p.Scopes, p.Ceiling = strings.Fields(scope), strings.Fields(ceiling)
	p.CreatedAt, p.ExpiresAt, p.Collected = time.Unix(created, 0), time.Unix(expires, 0), collected.Valid
	return p, nil
}

func getPending(ctx context.Context, q querier, id string) (Pending, error) {
	p, err := scanPending(q.QueryRowContext(ctx, `SELECT `+pendingCols+` FROM oauth_pending WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Pending{}, ErrUnknownPending
	}
	return p, err
}

func (s *Store) GetPending(ctx context.Context, id string) (Pending, error) {
	return getPending(ctx, s.db, id)
}

// ListPending is what `knomit oauth pending` shows: undecided, unexpired,
// oldest first.
func (s *Store) ListPending(ctx context.Context) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pendingCols+` FROM oauth_pending
WHERE decision = '' AND expires_at > ? ORDER BY created_at, id`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		p, err := scanPending(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Decide records an operator's decision on an undecided, unexpired request.
func (s *Store) Decide(ctx context.Context, id, decision, subject string, ceiling []string, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := getPending(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.Decision != "" {
		return ErrNotPending
	}
	now := s.now()
	if p.Expired(now) {
		return ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_pending SET decision = ?, subject = ?, ceiling = ?, decided_by = ?, decided_at = ?
WHERE id = ?`, decision, subject, strings.Join(ceiling, " "), by, now.Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Collect completes a DECIDED request, once. For an approval it starts the
// family and mints the code — here, and not at approval, so that the only
// party ever holding the plaintext code is the browser the redirect goes
// to. For a denial it returns no code. Either way the request is marked
// collected and cannot be completed again. An approval collected after the
// request's own expiry is refused: the operator approved a request the
// browser had already abandoned.
func (s *Store) Collect(ctx context.Context, id string) (string, Pending, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Pending{}, err
	}
	defer tx.Rollback()
	p, err := getPending(ctx, tx, id)
	if err != nil {
		return "", Pending{}, err
	}
	switch {
	case p.Collected:
		return "", p, ErrCollected
	case p.Decision == "":
		return "", p, ErrNotPending
	}
	now := s.now()
	if p.Expired(now) {
		return "", p, ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_pending SET collected_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
		return "", Pending{}, err
	}
	var code string
	if p.Decision == DecisionApproved {
		f, err := s.createFamilyTx(ctx, tx, FamilySpec{ClientID: p.ClientID, Subject: p.Subject, Scopes: p.Ceiling, Resource: p.Resource})
		if err != nil {
			return "", Pending{}, err
		}
		if code, err = newSecret(); err != nil {
			return "", Pending{}, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_codes (hash, family, client_id, redirect_uri, code_challenge, resource, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, hashSecret(code), f.ID, p.ClientID, p.RedirectURI, p.CodeChallenge, p.Resource,
			now.Unix(), now.Add(codeTTL).Unix()); err != nil {
			return "", Pending{}, err
		}
	}
	p.Collected = true
	return code, p, tx.Commit()
}

// Code is a consumed authorization code and everything it was bound to.
type Code struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Family        Family
}

// ConsumeCode spends a code, once. The caller checks the client, redirect
// URI, PKCE verifier and resource AFTER this: a failed exchange has still
// spent the code, so a verifier cannot be guessed at repeatedly. A code
// presented again after it was spent revokes its family — whatever the
// first exchange produced is in hands it should not be (RFC 6749 §4.1.2).
func (s *Store) ConsumeCode(ctx context.Context, code string) (Code, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Code{}, err
	}
	defer tx.Rollback()
	var (
		c        Code
		exp      int64
		consumed sql.NullInt64
	)
	f, err := scanFamily(tx.QueryRowContext(ctx, `
SELECT `+familyCols+`, c.client_id, c.redirect_uri, c.code_challenge, c.resource, c.expires_at, c.consumed_at
FROM oauth_codes c JOIN oauth_families f ON f.id = c.family
WHERE c.hash = ?`, hashSecret(code)).Scan, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Resource, &exp, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return Code{}, ErrUnknownToken
	}
	if err != nil {
		return Code{}, err
	}
	c.Family = f
	if f.Revoked {
		return Code{}, ErrRevoked
	}
	now := s.now()
	if consumed.Valid {
		if err := s.revokeFamilyTx(ctx, tx, f.ID); err != nil {
			return Code{}, err
		}
		if err := tx.Commit(); err != nil {
			return Code{}, err
		}
		return Code{}, ErrReused
	}
	if !now.Before(time.Unix(exp, 0)) {
		return Code{}, ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET consumed_at = ? WHERE hash = ?`, now.Unix(), hashSecret(code)); err != nil {
		return Code{}, err
	}
	return c, tx.Commit()
}
