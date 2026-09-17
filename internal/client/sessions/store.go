package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ysmood/goob"
)

// Store is the sole owner of the client_sessions table. It borrows the
// control.db handle (SetMaxOpenConns(1)); every method is one or two short
// statements, and callers on the MCP path invoke it AFTER the MCP response
// has been written so it never sits in the request's critical path.
type Store struct {
	db     *sql.DB
	policy Policy
	// ob is the change hub, nil unless WithHub attached one. See hub.go.
	ob *goob.Observable
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
	// The session id is client-supplied and becomes the PRIMARY KEY, so it is
	// capped like every other declared value — an unbounded header must not
	// become an unbounded row key.
	o.SessionID = Cap(o.SessionID)
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
  last_seen_at = excluded.last_seen_at, request_count = request_count + 1,
  -- A DELETE is the client SAYING it is done, not proof that it is: the
  -- default mcp-go manager keeps accepting the id afterwards. A later request
  -- revives the row rather than leaving it reading "ended" while the session
  -- is demonstrably still calling.
  ended_at = NULL`,
			o.SessionID, Cap(c.InstanceID), Cap(transport), o.Binding, Cap(c.Branch), Cap(c.Host), Cap(c.User), Cap(c.Cwd),
			c.PID, Cap(c.ParentApp), c.ParentPID, Cap(c.Version), Cap(o.RemoteIP), Cap(o.UserAgent), now, now)
		if err != nil {
			return err
		}
		s.publish(o.SessionID, "touch")
		return nil
	}

	// Direct HTTP caller: identity from what we can observe plus whatever
	// initialize declared (known only if SetClientInfo already ran).
	var name, version string
	err := s.db.QueryRowContext(ctx,
		`SELECT client_name, client_version FROM client_sessions WHERE id = ?`, o.SessionID).Scan(&name, &version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Cap BEFORE deriving, and store the same capped values: SetClientInfo
	// derives the identical id from the identical inputs, so the two writers
	// can never disagree about one session's instance id.
	ip, ua := Cap(o.RemoteIP), Cap(o.UserAgent)
	inst := DeriveInstanceID(ip, ua, name, version)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO client_sessions
  (id, instance_id, transport, binding, remote_addr, user_agent, first_seen_at, last_seen_at, request_count)
VALUES (?, ?, 'http', ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
  instance_id = excluded.instance_id,
  binding = CASE WHEN excluded.binding = '' THEN binding ELSE excluded.binding END,
  remote_addr = excluded.remote_addr, user_agent = excluded.user_agent,
  last_seen_at = excluded.last_seen_at, request_count = request_count + 1,
  -- A DELETE is the client SAYING it is done, not proof that it is: the
  -- default mcp-go manager keeps accepting the id afterwards. A later request
  -- revives the row rather than leaving it reading "ended" while the session
  -- is demonstrably still calling.
  ended_at = NULL`,
		o.SessionID, inst, o.Binding, ip, ua, now, now)
	if err != nil {
		return err
	}
	s.publish(o.SessionID, "touch")
	return nil
}

// SetClientInfo records what `initialize` declared, plus what the server
// OBSERVED about the same request.
//
// It takes remoteIP and userAgent rather than reading them back off the row
// because on a direct-HTTP session this is the FIRST write to that row:
// initialize carries no session id in its request, so no Touch has run, and
// the row it would read back still has ” for both. Deriving from ” would
// collapse every client declaring the same clientInfo onto one instance id.
//
// The row may not exist yet; it is created with transport "http" and refined
// by the next Touch. A row that already declares stdio keeps the identity the
// bridge declared — the server-derived id is only ever used for http.
func (s *Store) SetClientInfo(ctx context.Context, sessionID, binding, name, version, remoteIP, userAgent string, now time.Time) error {
	if sessionID == "" {
		return nil
	}
	sessionID = Cap(sessionID)
	name, version = Cap(name), Cap(version)
	ip, ua := Cap(remoteIP), Cap(userAgent)
	ts := now.Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO client_sessions
  (id, instance_id, transport, binding, remote_addr, user_agent, client_name, client_version, initialized, first_seen_at, last_seen_at, request_count)
VALUES (?, ?, 'http', ?, ?, ?, ?, ?, 1, ?, ?, 0)
ON CONFLICT(id) DO UPDATE SET
  binding = CASE WHEN excluded.binding = '' THEN binding ELSE excluded.binding END,
  client_name = excluded.client_name, client_version = excluded.client_version, initialized = 1,
  remote_addr = CASE WHEN excluded.remote_addr = '' THEN remote_addr ELSE excluded.remote_addr END,
  user_agent = CASE WHEN excluded.user_agent = '' THEN user_agent ELSE excluded.user_agent END,
  -- Only an http row carries a server-DERIVED id; a stdio row's id was
  -- declared by the bridge and must survive.
  instance_id = CASE WHEN client_sessions.transport = 'http' THEN excluded.instance_id ELSE instance_id END`,
		sessionID, DeriveInstanceID(ip, ua, name, version), binding, ip, ua, name, version, ts, ts)
	if err != nil {
		return err
	}
	s.publish(sessionID, "init")
	return nil
}

// End records an explicit session termination (HTTP DELETE). Unknown ids
// are a no-op: a DELETE for a session this server never recorded is not an
// error worth a log line.
func (s *Store) End(ctx context.Context, sessionID string, at time.Time) error {
	if sessionID == "" {
		return nil
	}
	sessionID = Cap(sessionID)
	res, err := s.db.ExecContext(ctx,
		`UPDATE client_sessions SET ended_at = ?, last_seen_at = ? WHERE id = ? AND ended_at IS NULL`,
		at.Unix(), at.Unix(), sessionID)
	if err != nil {
		return err
	}
	// The no-op cases this guard catches are exactly the ones the doc comment
	// above names: an id this server never recorded, and an id already ended.
	if n, _ := res.RowsAffected(); n > 0 {
		s.publish(sessionID, "end")
	}
	return nil
}

// Filter selects rows for List.
type Filter struct {
	// Binding matches a session if ANY binding it has presented is this pin —
	// its last-seen one, or any handle in client_session_bindings. One session
	// can serve several jobs at once, so "the session's binding" is a set, and
	// a filter that looked only at the last-seen column would hide a session
	// that is actively using this repo through a handle whose turn has passed.
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
		// EXISTS, not a JOIN: a session with three handles on this pin is ONE
		// session and must appear once. A join would return it three times and
		// every count built on this list would be wrong.
		q += ` AND (binding = ? OR EXISTS (
                  SELECT 1 FROM client_session_bindings b
                  WHERE b.session_id = client_sessions.id AND b.binding = ?))`
		args = append(args, f.Binding, f.Binding)
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

// Purge deletes rows whose last_seen_at is older than the retention window,
// then ages out binding handles unused for that same window. A handle is
// therefore durable across restarts but not beyond the retention window
// (session.client_retention, default 168h — configurable, not a constant): a
// caller that has not used a handle for that long must call knomit_bind again.
// Returns the number of client_sessions rows deleted. Retention 0 disables
// purging.
//
// Handles age on their OWN last_used_at, not on any session's. They are not
// keyed by session id — that is the whole point of the handle — so there is no
// session row whose death could collect them, and a handle held by a long-lived
// caller stays alive precisely as long as it keeps being used.
func (s *Store) Purge(ctx context.Context, now time.Time) (int64, error) {
	if s.policy.Retention <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_sessions WHERE last_seen_at < ?`,
		now.Add(-s.policy.Retention).Unix())
	if err != nil {
		return 0, fmt.Errorf("purge client_sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM binding_handles WHERE last_used_at < ?`,
		now.Add(-s.policy.Retention).Unix()); err != nil {
		return n, fmt.Errorf("purge binding_handles: %w", err)
	}
	// The binding SET belongs to its session and dies with it. Unlike
	// binding_handles, which is routing state a live caller may still present
	// and therefore ages on its own last-used time, these rows are only a
	// description of a session — once the session row is gone they describe
	// nothing. No foreign key, so the orphans are collected here.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM client_session_bindings WHERE session_id NOT IN (SELECT id FROM client_sessions)`); err != nil {
		return n, fmt.Errorf("purge client_session_bindings: %w", err)
	}
	// No id: a purge is not about one row, and the consumer re-reads the
	// whole list anyway.
	if n > 0 {
		s.publish("", "purge")
	}
	return n, nil
}
