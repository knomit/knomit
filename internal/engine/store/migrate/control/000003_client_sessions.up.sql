-- client_sessions: every MCP client session seen by this server, keyed by
-- Mcp-Session-Id. OPERATIONAL state, not knowledge — lives in control.db
-- (durable, global) rather than the per-repo *.sessions.db sidecar, which is
-- per repo (a lens session has no single repo) and wiped on every open.
-- Presence (live/idle/dead/hidden) is DERIVED at read time from last_seen_at;
-- no writer ever flips a status. Rows outlive their repo/lens on purpose: no
-- foreign key, purge is by time only (session.client_retention).
CREATE TABLE IF NOT EXISTS client_sessions (
    id              TEXT PRIMARY KEY,
    instance_id     TEXT NOT NULL,
    transport       TEXT NOT NULL,
    binding         TEXT NOT NULL,
    branch          TEXT NOT NULL DEFAULT '',
    hostname        TEXT NOT NULL DEFAULT '',
    username        TEXT NOT NULL DEFAULT '',
    cwd             TEXT NOT NULL DEFAULT '',
    pid             INTEGER NOT NULL DEFAULT 0,
    parent_app      TEXT NOT NULL DEFAULT '',
    parent_pid      INTEGER NOT NULL DEFAULT 0,
    bridge_version  TEXT NOT NULL DEFAULT '',
    remote_addr     TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '',
    client_name     TEXT NOT NULL DEFAULT '',
    client_version  TEXT NOT NULL DEFAULT '',
    initialized     INTEGER NOT NULL DEFAULT 0,
    principal       TEXT,
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    ended_at        INTEGER,
    request_count   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS client_sessions_last_seen ON client_sessions(last_seen_at);
CREATE INDEX IF NOT EXISTS client_sessions_binding ON client_sessions(binding, last_seen_at);
