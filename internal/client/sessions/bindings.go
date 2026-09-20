package sessions

// binding_handles is ROUTING state the client asserted through knomit_bind,
// unlike every column of client_sessions, which is observed. Keep the two
// apart: nothing in client_sessions may gate, this table decides which repo a
// tool call is served from.
//
// The key is a per-bind opaque handle, NOT the MCP session id. A client may
// share one connection — and therefore one session id — across several logical
// jobs, so session-keyed routing let two jobs overwrite each other's binding
// and served writes from the wrong repo. MCP 2026-07-28 removed protocol
// sessions and says cross-call state belongs in server-minted handles passed
// as ordinary tool arguments (SEP-2567).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// handleBytes is the entropy in a minted handle. 24 bytes is 192 bits, well
// past the 128-bit floor, and encodes to exactly 32 URL-safe characters with no
// padding — short enough that an agent carries it through a long session
// without truncating it.
const handleBytes = 24

// NewBindingHandle mints a fresh handle. It is random and carries NO structure:
// not the repo name, not the lens name, not the uid, not a counter. That is the
// point — a model cannot derive a valid handle from a name it has seen, so an
// invented `binding` argument is refused rather than silently routed somewhere.
//
// crypto/rand, not math/rand: a predictable handle would let one caller name
// another's binding, which is the failure this whole mechanism exists to stop.
func NewBindingHandle() (string, error) {
	buf := make([]byte, handleBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint binding handle: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// MintBindingHandle records that handle routes to pin ("repo:<uid>" or
// "lens:<uid>") at branch.
//
// branch is "" for every handle knomit_bind mints today, meaning "the target's
// own read branch" — the agent branch for a normal repo, the followed upstream
// for a subscription, which is what NewBindingOfRepo(ri, "") resolves. The
// parameter exists because per-handle branch switching is planned and the
// column is already there; see the migration.
//
// INSERT, not upsert. Each knomit_bind call mints its own handle, so a
// collision would mean crypto/rand repeated 192 bits — and quietly rewriting an
// existing handle's target is exactly the overwrite this design removes.
func (s *Store) MintBindingHandle(ctx context.Context, handle, pin, branch string, now time.Time) error {
	if handle == "" || pin == "" {
		return errors.New("mint binding handle: empty handle or pin")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO binding_handles (handle, binding, branch, created_at, last_used_at) VALUES (?, ?, ?, ?, ?)`,
		handle, pin, branch, now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("mint binding handle: %w", err)
	}
	return nil
}

// BindingHandle returns the pin and branch handle routes to; ok is false when no
// such handle exists. It also stamps last_used_at, which is what keeps a handle
// in active use alive while an abandoned one ages out of Purge's window.
//
// An empty branch means "the target's own read branch" and is what every handle
// carries today — see MintBindingHandle.
//
// A missing handle is ("", "", false, nil) — NOT an error. The caller turns it
// into the one tool error an agent can act on, and must not echo which handles
// do exist.
func (s *Store) BindingHandle(ctx context.Context, handle string, now time.Time) (pin, branch string, ok bool, err error) {
	if handle == "" {
		return "", "", false, nil
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT binding, branch FROM binding_handles WHERE handle = ?`, handle).Scan(&pin, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("binding handle: %w", err)
	}
	// Best effort: a failed touch costs the handle nothing until the retention
	// window, and failing the caller's tool call over bookkeeping would be a
	// worse trade than an early expiry.
	if _, uerr := s.db.ExecContext(ctx,
		`UPDATE binding_handles SET last_used_at = ? WHERE handle = ?`, now.Unix(), handle); uerr != nil {
		return pin, branch, true, nil
	}
	return pin, branch, true, nil
}

// HandleExperiment returns the experiment this handle is working inside, or ""
// when it is on the ordinary write branch. Absence of a row IS "", so there is
// no ok return: "not in an experiment" and "never set one" are the same state
// and nothing downstream distinguishes them.
//
// A read failure returns the error rather than "", because "" is a routing
// ANSWER here — it means "write to the agent branch" — and a caller that got
// it from a failed query would silently leave the experiment its user is
// working in. The gate turns the error into a tool error instead.
func (s *Store) HandleExperiment(ctx context.Context, handle string) (string, error) {
	if handle == "" {
		return "", nil
	}
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT experiment FROM handle_experiments WHERE handle = ?`, handle).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("handle experiment: %w", err)
	}
	return name, nil
}

// SetHandleExperiment records that handle is now working inside experiment.
//
// UPSERT, unlike MintBindingHandle's deliberate plain INSERT. The reasoning
// that makes an overwrite wrong there makes it right here: a handle's TARGET
// must never be silently rewritten, but a handle's experiment is a working
// context its own holder moves in and out of, and opening a second experiment
// from one session is a switch, not a collision.
func (s *Store) SetHandleExperiment(ctx context.Context, handle, experiment string, now time.Time) error {
	if handle == "" || experiment == "" {
		return errors.New("set handle experiment: empty handle or experiment")
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO handle_experiments (handle, experiment, set_at) VALUES (?, ?, ?)
ON CONFLICT(handle) DO UPDATE SET experiment = excluded.experiment, set_at = excluded.set_at`,
		handle, experiment, now.Unix()); err != nil {
		return fmt.Errorf("set handle experiment: %w", err)
	}
	return nil
}

// ClearHandleExperiment puts handle back on its ordinary write branch. A
// handle that was not in an experiment is not an error: commit and rollback
// both call this, and a caller repeating one is asking for a state it is
// already in.
func (s *Store) ClearHandleExperiment(ctx context.Context, handle string) error {
	if handle == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM handle_experiments WHERE handle = ?`, handle); err != nil {
		return fmt.Errorf("clear handle experiment: %w", err)
	}
	return nil
}
