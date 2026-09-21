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
	// TWO MECHANISMS, ONE ANSWER. binding_handles.branch and this table both
	// say "where does this handle point" for a repo pin. Rather than rank
	// them, the pair is made UNREPRESENTABLE at the write: an experiment may
	// only be set on a handle whose branch is empty, which is every handle
	// knomit_bind mints today. If per-handle branch switching ever lands, it
	// collides here loudly instead of silently losing to a precedence rule
	// nobody remembers.
	var branch string
	err := s.db.QueryRowContext(ctx,
		`SELECT branch FROM binding_handles WHERE handle = ?`, handle).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("set handle experiment: no such handle")
	}
	if err != nil {
		return fmt.Errorf("set handle experiment: read handle: %w", err)
	}
	if branch != "" {
		return fmt.Errorf(
			"set handle experiment: handle is pinned to branch %q; a handle cannot carry both a branch pin and an experiment",
			branch)
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

// MountExperiment reports the experiment a session is working inside on ONE
// URL-scoped mount, or "" for none.
//
// Keyed on (session id, mount uid), never on the session id alone: a client may
// hold a repo bridge and a lens bridge on one connection, and each carries its
// own experiment. See migration 000008 for why session-keying is admissible
// here and forbidden on the unscoped mount — the short version is that the URL
// has already fixed the repo, so this can only choose a BRANCH within it.
//
// A read failure returns the error, not "", for the reason HandleExperiment
// gives: "" is a routing ANSWER, so a caller handed it by a failed query would
// silently leave the experiment its user is working in.
func (s *Store) MountExperiment(ctx context.Context, sessionID, mountUID string) (string, error) {
	if sessionID == "" || mountUID == "" {
		return "", nil
	}
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT experiment FROM mount_experiments WHERE session_id = ? AND mount_uid = ?`,
		sessionID, mountUID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("mount experiment: %w", err)
	}
	return name, nil
}

// SetMountExperiment records that this session, on this mount, is now inside
// experiment. UPSERT for the same reason SetHandleExperiment upserts: opening a
// second experiment is a switch, not a collision.
//
// There is no branch-pin collision to guard against here, unlike the handle
// path: a URL-scoped mount's branch comes from the URL, and an experiment
// replaces it for the duration rather than competing with it.
func (s *Store) SetMountExperiment(ctx context.Context, sessionID, mountUID, experiment string, now time.Time) error {
	if sessionID == "" || mountUID == "" || experiment == "" {
		return errors.New("set mount experiment: empty session, mount or experiment")
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO mount_experiments (session_id, mount_uid, experiment, set_at) VALUES (?, ?, ?, ?)
ON CONFLICT(session_id, mount_uid) DO UPDATE SET experiment = excluded.experiment, set_at = excluded.set_at`,
		sessionID, mountUID, experiment, now.Unix()); err != nil {
		return fmt.Errorf("set mount experiment: %w", err)
	}
	return nil
}

// ClearMountExperiment puts this session, on this mount, back on the branch the
// URL names. Not an error when there was none: commit and rollback both call
// it, and repeating one asks for a state already held.
func (s *Store) ClearMountExperiment(ctx context.Context, sessionID, mountUID string) error {
	if sessionID == "" || mountUID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM mount_experiments WHERE session_id = ? AND mount_uid = ?`,
		sessionID, mountUID); err != nil {
		return fmt.Errorf("clear mount experiment: %w", err)
	}
	return nil
}

// MountExperimentRow is one URL-scoped mount this session is working inside an
// experiment on.
type MountExperimentRow struct {
	MountUID   string
	Experiment string
	SetAt      time.Time
}

// SessionMountExperiments lists the experiments a session holds across its
// URL-scoped mounts.
//
// This is the ATTRIBUTION read: it is how a session row can state the branch it
// actually writes to instead of the one its URL names. Sorted by mount so the
// listing is stable.
func (s *Store) SessionMountExperiments(ctx context.Context, sessionID string) ([]MountExperimentRow, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT mount_uid, experiment, set_at FROM mount_experiments WHERE session_id = ? ORDER BY mount_uid`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("session mount experiments: %w", err)
	}
	defer rows.Close()
	var out []MountExperimentRow
	for rows.Next() {
		var r MountExperimentRow
		var setAt int64
		if err := rows.Scan(&r.MountUID, &r.Experiment, &setAt); err != nil {
			return nil, fmt.Errorf("session mount experiments: scan: %w", err)
		}
		r.SetAt = time.Unix(setAt, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
