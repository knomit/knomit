-- session_bindings: the repo or lens an MCP session ASKED to be routed to,
-- keyed by Mcp-Session-Id, set by the knomit_bind tool on the unscoped
-- /api/v1/mcp mount. This is deliberately a separate table from
-- client_sessions: that table is an observational trail inferred from traffic
-- and must never gate anything, whereas this one is client-asserted ROUTING
-- state that the SessionBindingMiddleware acts on. The value is Binding.PinID
-- (`repo:<uid>` / `lens:<uid>`), never a name, so a rename leaves a live
-- session bound. Rows are durable across restarts on purpose: the stateless
-- session-id manager accepts ids across a restart, so a bridge that outlives
-- one keeps its selection. No foreign key: purge removes rows whose session
-- has aged out of client_sessions.
CREATE TABLE IF NOT EXISTS session_bindings (
    session_id TEXT PRIMARY KEY,
    binding    TEXT NOT NULL,
    set_at     INTEGER NOT NULL
);
