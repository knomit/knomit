package sessions

// client_session_bindings is the SET of binding handles one MCP session has
// presented, one row per (session, handle).
//
// It exists because `client_sessions.binding` cannot answer the question any
// more. That column holds the LAST pin seen, which described the session
// completely only while binding was per-session. Now a single session id can
// serve several independent jobs at once, each holding its own handle, so the
// last pin is a snapshot of whichever call happened to land most recently.
//
// Keyed by HANDLE, not by pin: two handles naming the same repo are two rows,
// because they are two callers, and collapsing them would discard exactly the
// distinction the handle exists to make.
//
// OBSERVATIONAL. Like every column of client_sessions, these rows are inferred
// from request traffic and must never gate anything — routing was decided by
// the handle argument on the call itself.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SessionBinding is one handle a session has been seen presenting.
type SessionBinding struct {
	Handle       string
	Binding      string // repo:<uid> | lens:<uid>
	Branch       string // "" ⇒ the target's own read branch
	FirstSeen    time.Time
	LastSeen     time.Time
	RequestCount int
}

// RecordSessionBinding notes that session sid presented handle, which resolved
// to pin at branch. Upsert: first sight inserts with count 1, every later sight
// bumps last_seen_at and the count.
//
// Called once per request that RESOLVED a handle, from the same seam that
// stamps client_sessions.binding — so a request that carried no handle, or one
// whose handle did not resolve, adds nothing. A row here means the handle was
// good at least once.
func (s *Store) RecordSessionBinding(ctx context.Context, sid, handle, pin, branch string, now time.Time) error {
	if sid == "" || handle == "" || pin == "" {
		return nil
	}
	ts := now.Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO client_session_bindings (session_id, handle, binding, branch, first_seen_at, last_seen_at, request_count)
VALUES (?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(session_id, handle) DO UPDATE SET
  -- binding and branch are refreshed rather than frozen: a handle's target is
  -- immutable today, but if per-handle rebinding ever lands the row must say
  -- what the handle means NOW, not what it meant on first sight.
  binding = excluded.binding, branch = excluded.branch,
  last_seen_at = excluded.last_seen_at, request_count = request_count + 1`,
		Cap(sid), Cap(handle), pin, Cap(branch), ts, ts)
	if err != nil {
		return fmt.Errorf("record session binding: %w", err)
	}
	return nil
}

// SessionBindings returns every recorded handle for each of sids, MOST RECENTLY
// USED FIRST within a session.
//
// ONE query for the whole page, deliberately. control.db runs at
// SetMaxOpenConns(1), so a lookup per session row would serialise the entire
// response behind a single connection — the same reason the handler builds one
// name index instead of resolving each row. An empty sids is not a query at
// all.
func (s *Store) SessionBindings(ctx context.Context, sids []string) (map[string][]SessionBinding, error) {
	out := map[string][]SessionBinding{}
	if len(sids) == 0 {
		return out, nil
	}
	ph := make([]string, len(sids))
	args := make([]any, len(sids))
	for i, id := range sids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, handle, binding, branch, first_seen_at, last_seen_at, request_count
FROM client_session_bindings
WHERE session_id IN (`+strings.Join(ph, ",")+`)
ORDER BY session_id, last_seen_at DESC, handle`, args...)
	if err != nil {
		return nil, fmt.Errorf("session bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var b SessionBinding
		var first, last int64
		if err := rows.Scan(&sid, &b.Handle, &b.Binding, &b.Branch, &first, &last, &b.RequestCount); err != nil {
			return nil, err
		}
		b.FirstSeen = time.Unix(first, 0)
		b.LastSeen = time.Unix(last, 0)
		out[sid] = append(out[sid], b)
	}
	return out, rows.Err()
}
