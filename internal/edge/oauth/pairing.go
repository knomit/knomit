package oauth

// Browser pairing (unified auth plan P3, Task 3.3; OPEN-12). A browser with
// no session asks to be paired: it gets a short code to show a person and a
// pending id only it knows. The request is an ordinary row of the
// oauth_pending queue whose client is the built-in WebClientID, plus an
// auth_pairing row for the code. Someone already signed in approves it by
// code or id; the waiting browser then COLLECTS the approval with its
// pending id and receives a session cookie directly — no authorization code
// to exchange (a same-origin first-party client gains nothing from one).
//
// The session is minted at COLLECT, not at approval, for the same reason an
// OAuth code is: the only party ever holding the plaintext cookie is the
// browser that receives it. It acts as the APPROVER's principal (§13.5).
//
// Pairing rows are invisible to every agent-queue path (getAgentPending),
// have their own live cap (maxLivePairing) so an anonymous flood cannot
// starve agent requests, and expire after Lifetimes.Pending.
//
// Built DARK: until the flip (P4) nothing calls ApprovePairing, and a
// session cookie grants nothing on the main port.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/foundation/authz"
)

// WebClientID is the built-in client id of knomit's own web UI. It is
// reserved: no configured or CIMD client may use it.
const WebClientID = "knomit-web"

// maxLivePairing bounds the undecided pairing requests at once. It is a cap
// of its own, beside maxLivePending: POST /auth/pair is public, and a flood
// of pairings must never stop an agent from being approved (nor the reverse).
const maxLivePairing = 20

// ErrDenied is a pairing that was denied.
var ErrDenied = errors.New("oauth: the pairing request was denied")

// pairingAlphabet has no 0/O or 1/I: a person reads the code off one screen
// and types it into another.
const pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newPairingCode() (string, error) {
	var b strings.Builder
	n := big.NewInt(int64(len(pairingAlphabet)))
	for i := range 6 {
		if i == 3 {
			b.WriteByte('-')
		}
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b.WriteByte(pairingAlphabet[k.Int64()])
	}
	return b.String(), nil
}

// normalizePairingCode turns what a person typed ("kq7m2x", "KQ7-M2X",
// " kq7-m2x ") into the stored spelling, or "" when it cannot be a code.
func normalizePairingCode(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
	if len(s) != 6 {
		return ""
	}
	for _, r := range s {
		if !strings.ContainsRune(pairingAlphabet, r) {
			return ""
		}
	}
	return s[:3] + "-" + s[3:]
}

// StartPairing parks a pairing request for a browser and returns its pending
// id (the browser's secret handle for /wait) and its code (what a person
// reads out). ErrTooManyPending when maxLivePairing are already waiting.
func (i *Issuer) StartPairing(ctx context.Context, ua, ip string) (pendingID, code string, _ error) {
	s := i.store
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now := s.now()
	if err := purgeExpiredPending(ctx, tx, now); err != nil {
		return "", "", err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_pending WHERE decision = '' AND client_id = ?`, WebClientID).Scan(&live); err != nil {
		return "", "", err
	}
	if live >= maxLivePairing {
		return "", "", ErrTooManyPending
	}
	if pendingID, err = newSecret(); err != nil {
		return "", "", err
	}
	ua, ip = truncate(ua, 512), truncate(ip, 64)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_pending (id, client_id, client_name, redirect_uri, scope, code_challenge, resource,
  state, remote_addr, user_agent, created_at, expires_at)
VALUES (?, ?, 'knomit web', '', '', '', '', '', ?, ?, ?, ?)`,
		pendingID, WebClientID, ip, ua, now.Unix(), now.Add(s.lt.Pending).Unix()); err != nil {
		return "", "", err
	}
	// A code collision among live pairings is possible (32^6 codes); draw
	// again rather than fail.
	for attempt := 0; ; attempt++ {
		if code, err = newPairingCode(); err != nil {
			return "", "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_pairing (pending_id, code, user_agent, ip) VALUES (?, ?, ?, ?)`, pendingID, code, ua, ip)
		if err == nil {
			break
		}
		if attempt >= 4 || !strings.Contains(err.Error(), "UNIQUE") {
			return "", "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	log.Info().Str("code", code).Str("remote", ip).Msg("oauth: browser pairing request waiting for approval")
	return pendingID, code, nil
}

// pairingRow is one pairing: its queue row and its auth_pairing row.
type pairingRow struct {
	Pending
	Code        string
	SessionHash string
}

// getPairing reads a pairing by pending id or code. Anything that is not a
// live pairing row is ErrUnknownPending.
func getPairing(ctx context.Context, q querier, idOrCode string) (pairingRow, error) {
	var where, arg string
	switch {
	case pendingIDRE.MatchString(idOrCode):
		where, arg = "a.pending_id = ?", idOrCode
	case normalizePairingCode(idOrCode) != "":
		where, arg = "a.code = ?", normalizePairingCode(idOrCode)
	default:
		return pairingRow{}, ErrUnknownPending
	}
	var r pairingRow
	p, err := scanPending(q.QueryRowContext(ctx, `SELECT `+prefixed("p.", pendingCols)+`
FROM auth_pairing a JOIN oauth_pending p ON p.id = a.pending_id
WHERE `+where+` AND p.client_id = ?`, arg, WebClientID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return pairingRow{}, ErrUnknownPending
	}
	if err != nil {
		return pairingRow{}, err
	}
	r.Pending = p
	if err := q.QueryRowContext(ctx, `SELECT code, session_hash FROM auth_pairing WHERE pending_id = ?`, p.ID).Scan(&r.Code, &r.SessionHash); err != nil {
		return pairingRow{}, err
	}
	return r, nil
}

// prefixed qualifies each column of a comma-separated list with p.
func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for k, c := range parts {
		parts[k] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// decidePairing records a decision on an undecided, unexpired pairing.
func (i *Issuer) decidePairing(ctx context.Context, idOrCode, decision string, by authz.Principal) (string, error) {
	if err := canonicalPrincipal(by); err != nil {
		return "", fmt.Errorf("oauth: a pairing is decided by a principal that can hold a session: %w", err)
	}
	s := i.store
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	r, err := getPairing(ctx, tx, idOrCode)
	if err != nil {
		return "", err
	}
	if r.Decision != "" {
		return "", ErrNotPending
	}
	now := s.now()
	if r.Expired(now) {
		return "", ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_pending SET decision = ?, decided_by = ?, decided_at = ? WHERE id = ? AND decision = ''`,
		decision, by.String(), now.Unix(), r.ID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	i.waiters.notify(r.ID)
	log.Info().Str("code", r.Code).Str("decision", decision).Str("by", by.String()).Msg("oauth: browser pairing decided")
	return r.ID, nil
}

// ApprovePairing approves the pairing named by its id or code on behalf of
// by. The waiting browser's session will act as by (§13.5), so by must be a
// principal that can hold a session (not anonymous, not a node).
func (i *Issuer) ApprovePairing(ctx context.Context, idOrCode string, by authz.Principal) error {
	_, err := i.decidePairing(ctx, idOrCode, DecisionApproved, by)
	return err
}

// DenyPairing denies the pairing named by its id or code.
func (i *Issuer) DenyPairing(ctx context.Context, idOrCode string, by authz.Principal) error {
	_, err := i.decidePairing(ctx, idOrCode, DecisionDenied, by)
	return err
}

// CollectPairing completes a decided pairing, once, by its pending id (never
// its code: the code is shown to people, the id only to the browser that
// asked). An approval mints the session here and returns its cookie; a
// denial returns ErrDenied. ErrNotPending while undecided, ErrCollected the
// second time, ErrExpired past the request's lifetime.
func (i *Issuer) CollectPairing(ctx context.Context, pendingID string) (string, error) {
	if !pendingIDRE.MatchString(pendingID) {
		return "", ErrUnknownPending
	}
	s := i.store
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	r, err := getPairing(ctx, tx, pendingID)
	if err != nil {
		return "", err
	}
	switch {
	case r.Collected:
		return "", ErrCollected
	case r.Decision == "":
		if r.Expired(s.now()) {
			return "", ErrExpired
		}
		return "", ErrNotPending
	}
	now := s.now()
	if r.Expired(now) {
		return "", ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_pending SET collected_at = ? WHERE id = ?`, now.Unix(), r.ID); err != nil {
		return "", err
	}
	if r.Decision != DecisionApproved {
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return "", ErrDenied
	}
	by, err := authz.ParsePrincipal(r.DecidedBy)
	if err != nil {
		return "", fmt.Errorf("oauth: pairing approver %q: %w", r.DecidedBy, err)
	}
	cookie, _, err := s.createSessionTx(ctx, tx, by, r.UserAgent)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_pairing SET session_hash = ? WHERE pending_id = ?`, hashSecret(cookie), r.ID); err != nil {
		return "", err
	}
	return cookie, tx.Commit()
}

// WaitPairing long-polls a pairing for at most the issuer's wait timeout and
// collects it once decided: the cookie on approval, ErrDenied on denial.
// ErrNotPending means "still waiting" (the page polls again). Like /wait, an
// id that names no pairing registers no waiter.
func (i *Issuer) WaitPairing(ctx context.Context, pendingID string) (string, error) {
	if !pendingIDRE.MatchString(pendingID) {
		return "", ErrUnknownPending
	}
	deadline := time.Now().Add(i.waitTimeout)
	for {
		r, err := getPairing(ctx, i.store.db, pendingID)
		if err != nil {
			return "", err
		}
		if r.Decision != "" || r.Collected {
			break
		}
		if r.Expired(i.store.now()) {
			return "", ErrExpired
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", ErrNotPending
		}
		ch, release := i.waiters.wait(pendingID)
		if q, err := getPairing(ctx, i.store.db, pendingID); err == nil && q.Decision != "" {
			release()
			break
		}
		t := time.NewTimer(remaining)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		release()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return i.CollectPairing(ctx, pendingID)
}
