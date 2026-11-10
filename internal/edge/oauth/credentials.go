package oauth

// Browser sessions, one-time login links and personal access tokens
// (unified auth plan P3, Task 3.2). Built DARK: until the flip (P4) nothing
// on the main port reads them, so holding one grants nothing yet.
//
// Every secret is a random string only the client holds; control.db keeps
// its SHA-256 (hashSecret) and nothing else. A lookup is by that hash, never
// by comparing plaintexts, so there is no comparison to time.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/foundation/authz"
)

var (
	// ErrInvalid is every refusal of a presented session cookie, login link
	// or PAT: unknown, revoked, expired, idle or already used. The caller
	// answers 401 for each alike and never falls back to anonymous.
	ErrInvalid = errors.New("oauth: credential is invalid, expired, revoked or already used")
	// ErrNotFound is an id (session, PAT) that names nothing live.
	ErrNotFound = errors.New("oauth: no such credential")
)

// touchEvery bounds how often LookupSession writes last_seen_at.
const touchEvery = time.Minute

// newID is 8 random bytes, hex: a short, URL-safe id for listing and
// revoking. It is NOT a secret (the secret is the cookie or the PAT).
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// canonicalPrincipal refuses a principal that is not in its one spelling,
// or one that can never hold a credential of its own (a node authenticates
// by its certificate, anonymous by having nothing).
func canonicalPrincipal(p authz.Principal) error {
	q, err := authz.ParsePrincipal(p.String())
	if err != nil {
		return err
	}
	if q.Kind != p.Kind || q.ID != p.ID {
		return fmt.Errorf("oauth: principal %q is not in its canonical spelling", p.String())
	}
	if p.Kind == authz.KindNode || p.Kind == authz.KindAnonymous {
		return fmt.Errorf("oauth: a %s principal cannot hold a session or link", p.Kind)
	}
	return nil
}

// --- sessions -----------------------------------------------------------------

// Session is one browser session. Principal is who it acts as: the principal
// that logged it in (a link's minter, or a pairing's approver, §13.5).
type Session struct {
	ID             string
	Principal      authz.Principal
	UserAgent      string
	Created        time.Time
	LastSeen       time.Time
	AbsoluteExpiry time.Time
}

// CreateSession starts a session for p and returns its cookie value, which
// exists only here and in the Set-Cookie that carries it.
func (s *Store) CreateSession(ctx context.Context, p authz.Principal, ua string) (string, Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Session{}, err
	}
	defer tx.Rollback()
	cookie, sess, err := s.createSessionTx(ctx, tx, p, ua)
	if err != nil {
		return "", Session{}, err
	}
	return cookie, sess, tx.Commit()
}

func (s *Store) createSessionTx(ctx context.Context, tx *sql.Tx, p authz.Principal, ua string) (string, Session, error) {
	if err := canonicalPrincipal(p); err != nil {
		return "", Session{}, err
	}
	now := s.now()
	// Dead sessions go first, so the table holds only live ones plus the
	// revoked/idle rows of the last absolute lifetime at most.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE absolute_expires_at <= ? OR last_seen_at <= ?`,
		now.Unix(), now.Add(-s.lt.SessionIdle).Unix()); err != nil {
		return "", Session{}, err
	}
	id, err := newID()
	if err != nil {
		return "", Session{}, err
	}
	cookie, err := newSecret()
	if err != nil {
		return "", Session{}, err
	}
	ua = truncate(ua, 512)
	sess := Session{ID: id, Principal: authz.Principal{Kind: p.Kind, ID: p.ID}, UserAgent: ua,
		Created: now, LastSeen: now, AbsoluteExpiry: now.Add(s.lt.SessionAbsolute)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions (id, hash, principal, user_agent, created_at, last_seen_at, absolute_expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, hashSecret(cookie), sess.Principal.String(), ua, now.Unix(), now.Unix(), sess.AbsoluteExpiry.Unix()); err != nil {
		return "", Session{}, err
	}
	return cookie, sess, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

const sessionCols = `id, principal, user_agent, created_at, last_seen_at, absolute_expires_at`

func scanSession(scan func(...any) error, extra ...any) (Session, error) {
	var (
		sess                      Session
		principal                 string
		created, seen, absoluteTo int64
	)
	if err := scan(append([]any{&sess.ID, &principal, &sess.UserAgent, &created, &seen, &absoluteTo}, extra...)...); err != nil {
		return Session{}, err
	}
	p, err := authz.ParsePrincipal(principal)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", errBadRow, err)
	}
	sess.Principal = p
	sess.Created, sess.LastSeen, sess.AbsoluteExpiry = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(absoluteTo, 0)
	return sess, nil
}

// LookupSession answers the live session the cookie names, or ErrInvalid:
// unknown, revoked, past its absolute expiry, or idle for SessionIdle since
// it was last seen. A live session's last_seen_at is moved to now at most
// once per minute.
func (s *Store) LookupSession(ctx context.Context, cookie string) (Session, error) {
	if cookie == "" {
		return Session{}, ErrInvalid
	}
	var revoked sql.NullInt64
	sess, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+`, revoked_at FROM auth_sessions WHERE hash = ?`,
		hashSecret(cookie)).Scan, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrInvalid
	}
	if err != nil {
		// A row whose principal no longer parses is no session.
		if errors.Is(err, errBadRow) {
			return Session{}, ErrInvalid
		}
		return Session{}, err
	}
	now := s.now()
	if revoked.Valid || !now.Before(sess.AbsoluteExpiry) || !now.Before(sess.LastSeen.Add(s.lt.SessionIdle)) {
		return Session{}, ErrInvalid
	}
	if now.Sub(sess.LastSeen) >= touchEvery {
		if _, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at = ? WHERE id = ?`, now.Unix(), sess.ID); err != nil {
			return Session{}, err
		}
		sess.LastSeen = time.Unix(now.Unix(), 0)
	}
	return sess, nil
}

// errBadRow is a row that scanned but whose content does not validate (a
// hand-edited principal): it names no live credential.
var errBadRow = errors.New("oauth: malformed credential row")

// ListSessions lists the live sessions, newest first.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM auth_sessions
WHERE revoked_at IS NULL AND absolute_expires_at > ? AND last_seen_at > ? ORDER BY created_at DESC, id`,
		now.Unix(), now.Add(-s.lt.SessionIdle).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows.Scan)
		if err != nil {
			log.Warn().Err(err).Msg("oauth: skipping a session row whose principal does not parse")
			continue
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// RevokeSession ends a live session at once (ErrNotFound when id names no
// live session).
func (s *Store) RevokeSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- one-time login links ----------------------------------------------------

// MintLink makes a single-use login link token for p, valid for Link.
func (s *Store) MintLink(ctx context.Context, p authz.Principal) (string, time.Time, error) {
	if err := canonicalPrincipal(p); err != nil {
		return "", time.Time{}, err
	}
	tok, err := newSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	exp := now.Add(s.lt.Link)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_login_links WHERE expires_at <= ?`, now.Unix()); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_login_links (hash, principal, expires_at) VALUES (?, ?, ?)`,
		hashSecret(tok), p.String(), exp.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, tx.Commit()
}

// ConsumeLink spends a link, once: one atomic UPDATE guarded by
// used_at IS NULL AND expires_at > now, so of two racing consumers exactly one
// wins. Every refusal is ErrInvalid.
func (s *Store) ConsumeLink(ctx context.Context, token string) (authz.Principal, error) {
	if token == "" {
		return authz.Principal{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return authz.Principal{}, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	h := hashSecret(token)
	res, err := tx.ExecContext(ctx, `UPDATE auth_login_links SET used_at = ? WHERE hash = ? AND used_at IS NULL AND expires_at > ?`, now, h, now)
	if err != nil {
		return authz.Principal{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return authz.Principal{}, ErrInvalid
	}
	var principal string
	if err := tx.QueryRowContext(ctx, `SELECT principal FROM auth_login_links WHERE hash = ?`, h).Scan(&principal); err != nil {
		return authz.Principal{}, err
	}
	p, err := authz.ParsePrincipal(principal)
	if err != nil {
		return authz.Principal{}, ErrInvalid
	}
	return p, tx.Commit()
}

// --- personal access tokens ----------------------------------------------------

// PATPrefix starts every personal access token, so the authenticator can
// tell one from an OAuth access token and a leaked one is recognisable.
const PATPrefix = "knomit_pat_"

// PAT is one personal access token, as listed. Its principal is token:<ID>.
type PAT struct {
	ID, Label string
	Role      authz.Role
	CreatedBy string
	Created   time.Time
	Expires   time.Time
	LastUsed  time.Time // zero when never used
}

// Principal is the token principal the PAT authenticates as.
func (p PAT) Principal() authz.Principal { return authz.Principal{Kind: authz.KindToken, ID: p.ID} }

// CreatePAT mints a PAT bound to exactly one role (a token role: reader,
// contributor or operator; never admin, guest or node) and returns its
// secret, which exists only in this return value. expires is required: after
// now and at most now+PATMax. The binding token:<id> → role is written in the
// authz store right after the row; if it fails the row is deleted, and until
// it lands the token materializes to nothing (fail closed).
func (s *Store) CreatePAT(ctx context.Context, role authz.Role, label string, expires time.Time, by authz.Principal) (string, PAT, error) {
	if s.bindings == nil {
		return "", PAT{}, errors.New("oauth: this store has no bindings store; PATs cannot be minted")
	}
	if !slices.Contains(authz.TokenRoles(), role) {
		return "", PAT{}, fmt.Errorf("oauth: a token's role must be one of %v, not %q", authz.TokenRoles(), role)
	}
	now := s.now()
	switch {
	case expires.IsZero():
		return "", PAT{}, errors.New("oauth: a personal access token needs an expiry")
	case !expires.After(now):
		return "", PAT{}, errors.New("oauth: the expiry must be in the future")
	case expires.After(now.Add(s.lt.PATMax)):
		return "", PAT{}, fmt.Errorf("oauth: the expiry may be at most %v from now", s.lt.PATMax)
	}
	if _, err := authz.ParsePrincipal(by.String()); err != nil {
		return "", PAT{}, fmt.Errorf("oauth: creator: %w", err)
	}
	label = truncate(strings.TrimSpace(label), 200)
	id, err := newID()
	if err != nil {
		return "", PAT{}, err
	}
	raw, err := newSecret()
	if err != nil {
		return "", PAT{}, err
	}
	secret := PATPrefix + raw
	pat := PAT{ID: id, Label: label, Role: role, CreatedBy: by.String(), Created: time.Unix(now.Unix(), 0), Expires: time.Unix(expires.Unix(), 0)}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_tokens (id, hash, label, role, created_by, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, hashSecret(secret), label, string(role), pat.CreatedBy, now.Unix(), expires.Unix()); err != nil {
		return "", PAT{}, err
	}
	if err := s.bindings.Bind(ctx, authz.Binding{Principal: pat.Principal(), Role: role, Label: label, By: pat.CreatedBy, At: now}); err != nil {
		if _, derr := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE id = ?`, id); derr != nil {
			log.Error().Err(derr).Str("id", id).Msg("oauth: could not remove a PAT whose binding failed; it materializes to nothing")
		}
		return "", PAT{}, err
	}
	return secret, pat, nil
}

const patCols = `id, label, role, created_by, created_at, expires_at, COALESCE(last_used_at, 0)`

func scanPAT(scan func(...any) error, extra ...any) (PAT, error) {
	var (
		p                      PAT
		role                   string
		created, expires, used int64
	)
	if err := scan(append([]any{&p.ID, &p.Label, &role, &p.CreatedBy, &created, &expires, &used}, extra...)...); err != nil {
		return PAT{}, err
	}
	p.Role = authz.Role(role)
	p.Created, p.Expires = time.Unix(created, 0), time.Unix(expires, 0)
	if used != 0 {
		p.LastUsed = time.Unix(used, 0)
	}
	return p, nil
}

// VerifyPAT answers the live PAT secret names, or ErrInvalid (no prefix,
// unknown, revoked, expired). There is no issuer check: a token is valid on
// every external address (§15.2). last_used_at moves at most once a minute.
func (s *Store) VerifyPAT(ctx context.Context, secret string) (PAT, error) {
	if !strings.HasPrefix(secret, PATPrefix) || len(secret) == len(PATPrefix) {
		return PAT{}, ErrInvalid
	}
	var revoked sql.NullInt64
	p, err := scanPAT(s.db.QueryRowContext(ctx, `SELECT `+patCols+`, revoked_at FROM auth_tokens WHERE hash = ?`, hashSecret(secret)).Scan, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return PAT{}, ErrInvalid
	}
	if err != nil {
		return PAT{}, err
	}
	now := s.now()
	if revoked.Valid || !now.Before(p.Expires) {
		return PAT{}, ErrInvalid
	}
	if p.LastUsed.IsZero() || now.Sub(p.LastUsed) >= touchEvery {
		if _, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), p.ID); err != nil {
			return PAT{}, err
		}
		p.LastUsed = time.Unix(now.Unix(), 0)
	}
	return p, nil
}

// ListPATs lists the PATs that can still be used, newest first.
func (s *Store) ListPATs(ctx context.Context) ([]PAT, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+patCols+` FROM auth_tokens
WHERE revoked_at IS NULL AND expires_at > ? ORDER BY created_at DESC, id`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PAT{}
	for rows.Next() {
		p, err := scanPAT(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokePAT revokes a PAT and removes its role binding (UnbindAll on
// token:<id>), so it stops working on the very next request. ErrNotFound
// when id names no unrevoked PAT.
func (s *Store) RevokePAT(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if s.bindings != nil {
		return s.bindings.UnbindAll(ctx, PAT{ID: id}.Principal())
	}
	return nil
}
