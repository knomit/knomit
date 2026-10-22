-- session_bindings: the repo or lens an MCP session ASKED to be routed to,
-- keyed by Mcp-Session-Id, set by the knomit_bind tool on the unscoped
-- /api/v1/mcp mount. This is deliberately a separate table from
-- client_sessions: that table is an observational trail inferred from traffic
-- and must never gate anything, whereas this one is client-asserted ROUTING
-- state that the SessionBindingMiddleware acts on. The value is Binding.PinID
-- (`repo:<uid>` / `lens:<uid>`), never a name, so a rename leaves a live
-- session bound. Rows are durable across restarts on purpose: the stateless
-- session-id manager accepts ids across a restart, so a bridge that outlives
-- one keeps its selection. Durable across restarts is the whole claim, not
-- durable forever: no foreign key, and a row is purged together with its
-- client_sessions row once that session passes the retention window
-- (session.client_retention, default 168h — a SETTING, not a constant), so a
-- session idle longer than that must call knomit_bind again.
CREATE TABLE IF NOT EXISTS session_bindings (
    session_id TEXT PRIMARY KEY,
    binding    TEXT NOT NULL,
    set_at     INTEGER NOT NULL
);
