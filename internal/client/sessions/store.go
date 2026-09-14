package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store is the sole owner of the client_sessions table. It borrows the
// control.db handle (SetMaxOpenConns(1)); every method is one or two short
// statements, and callers on the MCP path invoke it AFTER the MCP response
// has been written so it never sits in the request's critical path.
type Store struct {
	db     *sql.DB
	policy Policy
}

// New returns a Store over an already-migrated control.db handle.
func New(db *sql.DB, p Policy) *Store { return &Store{db: db, policy: p} }

// Policy exposes the thresholds so handlers can echo them instead of
// hardcoding their own.
func (s *Store) Policy() Policy { return s.policy }

// Observation is one MCP request as seen by the HTTP dispatch layer.
type Observation struct {
	SessionID string // Mcp-Session-Id; "" ⇒ Touch is a no-op
	Binding   string // repo:<uid> | lens:<uid> | ""
	RemoteIP  string // host part only
	UserAgent string
	Client    *BridgeInfo // parsed X-Knomit-Client; nil when absent/unparseable
	Now       time.Time
}

// Touch records one request: insert on first sight, otherwise bump
// last_seen_at and request_count. With declared client info every declared
// column is refreshed (a session resumed after a server restart fills in on
// its first post-restart request). Without it the row is transport "http"
// and instance_id is derived from what the server can observe.
func (s *Store) Touch(ctx context.Context, o Observation) error {
	if o.SessionID == "" {
		return nil
	}
	now := o.Now.Unix()
	if o.Client != nil {
		c := o.Client
		transport := c.Transport
		if transport == "" {
			transport = "stdio"
		}
		_, err := s.db.ExecContext(ctx, `
INSERT INTO client_sessions
  (id, instance_id, transport, binding, branch, hostname, username, cwd, pid, parent_app, parent_pid,
   bridge_version, remote_addr, user_agent, first_seen_at, last_seen_at, request_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
  instance_id = excluded.instance_id, transport = excluded.transport,
  binding = CASE WHEN excluded.binding = '' THEN binding ELSE excluded.binding END,
  branch = excluded.branch, hostname = excluded.hostname, username = excluded.username,
  cwd = excluded.cwd, pid = excluded.pid, parent_app = excluded.parent_app, parent_pid = excluded.parent_pid,
  bridge_version = excluded.bridge_version, remote_addr = excluded.remote_addr, user_agent = excluded.user_agent,
  last_seen_at = excluded.last_seen_at, request_count = request_count + 1`,
			o.SessionID, Cap(c.InstanceID), Cap(transport), o.Binding, Cap(c.Branch), Cap(c.Host), Cap(c.User), Cap(c.Cwd),
			c.PID, Cap(c.ParentApp), c.ParentPID, Cap(c.Version), Cap(o.RemoteIP), Cap(o.UserAgent), now, now)
		return err
	}

	// Direct HTTP caller: identity from what we can observe plus whatever
	// initialize declared (known only if SetClientInfo already ran).
	var name, version string
	err := s.db.QueryRowContext(ctx,
		`SELECT client_name, client_version FROM client_sessions WHERE id = ?`, o.SessionID).Scan(&name, &version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	inst := DeriveInstanceID(o.RemoteIP, o.UserAgent, name, version)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO client_sessions
  (id, instance_id, transport, binding, remote_addr, user_agent, first_seen_at, last_seen_at, request_count)
VALUES (?, ?, 'http', ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
  instance_id = excluded.instance_id,
  binding = CASE WHEN excluded.binding = '' THEN binding ELSE excluded.binding END,
  remote_addr = excluded.remote_addr, user_agent = excluded.user_agent,
  last_seen_at = excluded.last_seen_at, request_count = request_count + 1`,
		o.SessionID, inst, o.Binding, Cap(o.RemoteIP), Cap(o.UserAgent), now, now)
	return err
}

// SetClientInfo records what `initialize` declared. The row may not exist
// yet (initialize carries no session id in its request, so no Touch preceded
// it); it is created with transport "http" and refined by the next Touch.
// For an http row the derived instance id is recomputed here so it includes
// the client name — after this it is stable.
func (s *Store) SetClientInfo(ctx context.Context, sessionID, binding, name, version string, now time.Time) error {
	if sessionID == "" {
		return nil
	}
	name, version = Cap(name), Cap(version)
	ts := now.Unix()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO client_sessions
  (id, instance_id, transport, binding, client_name, client_version, initialized, first_seen_at, last_seen_at, request_count)
VALUES (?, '', 'http', ?, ?, ?, 1, ?, ?, 0)
ON CONFLICT(id) DO UPDATE SET
  binding = CASE WHEN excluded.binding = '' THEN binding ELSE excluded.binding END,
  client_name = excluded.client_name, client_version = excluded.client_version, initialized = 1`,
		sessionID, binding, name, version, ts, ts); err != nil {
		return err
	}
	var transport, ip, ua string
	if err := s.db.QueryRowContext(ctx,
		`SELECT transport, remote_addr, user_agent FROM client_sessions WHERE id = ?`, sessionID).Scan(&transport, &ip, &ua); err != nil {
		return err
	}
	if transport != "http" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE client_sessions SET instance_id = ? WHERE id = ?`,
		DeriveInstanceID(ip, ua, name, version), sessionID)
	return err
}

// End records an explicit session termination (HTTP DELETE). Unknown ids
// are a no-op: a DELETE for a session this server never recorded is not an
// error worth a log line.
func (s *Store) End(ctx context.Context, sessionID string, at time.Time) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE client_sessions SET ended_at = ?, last_seen_at = ? WHERE id = ? AND ended_at IS NULL`,
		at.Unix(), at.Unix(), sessionID)
	return err
}

// Filter selects rows for List.
type Filter struct {
	Binding       string // "" = all
	IncludeHidden bool   // include rows silent longer than Policy.HiddenAfter
	Now           time.Time
}

// Session is one row with its read-time State.
type Session struct {
	ID, InstanceID, Transport, Binding, Branch string
	Host, User, Cwd, ParentApp, BridgeVersion  string
	PID, ParentPID                             int
	RemoteAddr, UserAgent                      string
	ClientName, ClientVersion                  string
	Initialized                                bool
	FirstSeen, LastSeen                        time.Time
	Ended                                      *time.Time
	RequestCount                               int
	State                                      State
}

// List returns sessions newest-last-seen first.
func (s *Store) List(ctx context.Context, f Filter) ([]Session, error) {
	q := `SELECT id, instance_id, transport, binding, branch, hostname, username, cwd, pid, parent_app, parent_pid,
       bridge_version, remote_addr, user_agent, client_name, client_version, initialized,
       first_seen_at, last_seen_at, ended_at, request_count
FROM client_sessions WHERE 1=1`
	args := []any{}
	if f.Binding != "" {
		q += ` AND binding = ?`
		args = append(args, f.Binding)
	}
	if !f.IncludeHidden {
		q += ` AND last_seen_at >= ?`
		args = append(args, f.Now.Add(-s.policy.HiddenAfter).Unix())
	}
	q += ` ORDER BY last_seen_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var r Session
		var first, last int64
		var ended sql.NullInt64
		var initialized int
		if err := rows.Scan(&r.ID, &r.InstanceID, &r.Transport, &r.Binding, &r.Branch, &r.Host, &r.User, &r.Cwd,
			&r.PID, &r.ParentApp, &r.ParentPID, &r.BridgeVersion, &r.RemoteAddr, &r.UserAgent,
			&r.ClientName, &r.ClientVersion, &initialized, &first, &last, &ended, &r.RequestCount); err != nil {
			return nil, err
		}
		r.Initialized = initialized == 1
		r.FirstSeen = time.Unix(first, 0)
		r.LastSeen = time.Unix(last, 0)
		if ended.Valid {
			t := time.Unix(ended.Int64, 0)
			r.Ended = &t
		}
		r.State = s.policy.StateAt(r.LastSeen, r.Ended != nil, f.Now)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Purge deletes rows whose last_seen_at is older than the retention window.
// Returns the number deleted. Retention 0 disables purging.
func (s *Store) Purge(ctx context.Context, now time.Time) (int64, error) {
	if s.policy.Retention <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_sessions WHERE last_seen_at < ?`,
		now.Add(-s.policy.Retention).Unix())
	if err != nil {
		return 0, fmt.Errorf("purge client_sessions: %w", err)
	}
	return res.RowsAffected()
}
