-- client_session_bindings: every binding HANDLE an MCP session has presented,
-- one row per (session, handle).
--
-- Keyed by handle, not by pin, and that is the point. Since binding became a
-- per-call handle, one session id can serve several independent jobs at once,
-- so `client_sessions.binding` -- which holds the LAST pin seen -- describes a
-- session only at the instant it was written. This table is the SET: two
-- handles naming the same repo are two rows, because they are two callers, and
-- collapsing them would throw away the distinction the handle exists to make.
--
-- `branch` mirrors binding_handles.branch: '' means the target's own read
-- branch. It is carried here so a reader can see what a handle resolved to
-- without a second lookup, and so the column is already present when per-handle
-- branch switching lands.
--
-- OBSERVATIONAL, like every column of client_sessions and for the same reason:
-- these rows are inferred from request traffic and must never gate anything.
-- Routing is decided by the handle argument on the call itself. A row here says
-- only "this session was seen presenting this handle", which is exactly what
-- makes it useful for the Sessions UI and useless as a permission.
--
-- No foreign key, matching client_sessions' neighbours. Rows are collected in
-- Store.Purge when their session ages out, so the set lives exactly as long as
-- the session row it describes.
CREATE TABLE IF NOT EXISTS client_session_bindings (
    session_id     TEXT NOT NULL,
    handle         TEXT NOT NULL,
    binding        TEXT NOT NULL,
    branch         TEXT NOT NULL DEFAULT '',
    first_seen_at  INTEGER NOT NULL,
    last_seen_at   INTEGER NOT NULL,
    request_count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, handle)
);
-- The list endpoint reads every row for a page of sessions in ONE query and the
-- ?binding= filter asks "does this session have a row with this pin", so both
-- access paths are by session_id and by binding respectively.
CREATE INDEX IF NOT EXISTS client_session_bindings_binding ON client_session_bindings(binding);
