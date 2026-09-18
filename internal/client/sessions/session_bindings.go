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

// BindingChunkSize bounds how many session ids go into one IN (...) clause.
//
// 900 is DELIBERATELY CONSERVATIVE and deliberately not measured. SQLite's
// SQLITE_MAX_VARIABLE_NUMBER is a COMPILE-TIME limit, not a property of SQLite:
// it is 32766 on the bundled 3.51.2, but it was 999 before 3.32 and a system or
// differently-built library can still be 999 today. A chunk size derived from
// measuring today's library is a constant that breaks on the one machine that
// differs, and breaks silently, because nothing re-measures. 900 is under the
// smallest limit anyone is likely to link.
//
// It is exported so a test can seed past the REAL boundary. A test-only small
// chunk size would exercise a different code path from production and prove
// nothing about it.
const BindingChunkSize = 900

// SessionBindings returns every recorded handle for each of sids, MOST RECENTLY
// USED FIRST within a session.
//
// One query PER CHUNK, not one per session. control.db runs at
// SetMaxOpenConns(1), so a lookup per session row would serialise the entire
// response behind a single connection — the same reason the handler builds one
// name index instead of resolving each row. Chunking keeps that property while
// bounding the variable count; Store.List's limit is what bounds the number of
// chunks. An empty sids is not a query at all.
func (s *Store) SessionBindings(ctx context.Context, sids []string) (map[string][]SessionBinding, error) {
	out := map[string][]SessionBinding{}
	for start := 0; start < len(sids); start += BindingChunkSize {
		end := start + BindingChunkSize
		if end > len(sids) {
			// The LAST PARTIAL CHUNK. Dropping it is the classic chunking bug
			// and it returns NO error — the sessions in it would simply come
			// back with empty sets, which reads downstream as "presented no
			// handles" and is indistinguishable from the truth.
			end = len(sids)
		}
		if err := s.appendBindings(ctx, sids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// appendBindings reads one chunk into out. Split from the loop so the query is
// written once and the chunking has nothing to get wrong but its indices.
func (s *Store) appendBindings(ctx context.Context, sids []string, out map[string][]SessionBinding) error {
	if len(sids) == 0 {
		return nil
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
		return fmt.Errorf("session bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var b SessionBinding
		var first, last int64
		if err := rows.Scan(&sid, &b.Handle, &b.Binding, &b.Branch, &first, &last, &b.RequestCount); err != nil {
			return err
		}
		b.FirstSeen = time.Unix(first, 0)
		b.LastSeen = time.Unix(last, 0)
		out[sid] = append(out[sid], b)
	}
	return rows.Err()
}
