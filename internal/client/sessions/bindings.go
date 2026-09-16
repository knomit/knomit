package sessions

// session_bindings is ROUTING state the client asserted through knomit_bind,
// unlike every column of client_sessions, which is observed. Keep the two
// apart: nothing in client_sessions may gate, this table is read by the
// SessionBindingMiddleware to pick the repo a request is served from.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BindSession records that session sid is bound to pin ("repo:<uid>" or
// "lens:<uid>"). Upsert: re-binding a session overwrites its previous pin.
func (s *Store) BindSession(ctx context.Context, sid, pin string, now time.Time) error {
	if sid == "" || pin == "" {
		return errors.New("bind session: empty session id or pin")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_bindings (session_id, binding, set_at) VALUES (?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET binding = excluded.binding, set_at = excluded.set_at`,
		Cap(sid), pin, now.Unix())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	// Nudge the Sessions UI's change stream so it re-reads the list now. Note
	// the client_sessions row still shows the PREVIOUS pin at this instant: its
	// binding column is written by Touch, from a context built before this
	// bind, so the new pin appears on the session's next request.
	s.publish(sid, "touch")
	return nil
}

// SessionBinding returns the pin bound to sid; ok is false when none is.
func (s *Store) SessionBinding(ctx context.Context, sid string) (string, bool, error) {
	if sid == "" {
		return "", false, nil
	}
	var pin string
	err := s.db.QueryRowContext(ctx,
		`SELECT binding FROM session_bindings WHERE session_id = ?`, Cap(sid)).Scan(&pin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session binding: %w", err)
	}
	return pin, true, nil
}
