-- binding_handles: the repo or lens a CALLER asked to be routed to, keyed by an
-- opaque handle that knomit_bind mints per call and every other tool passes
-- back as its `binding` argument.
--
-- It replaces session_bindings, which keyed the same routing state on
-- Mcp-Session-Id. That key was wrong, not merely inconvenient: a client may
-- share ONE connection — and therefore one session id — across several logical
-- jobs (Claude Desktop does this for Cowork sessions), so two jobs' knomit_bind
-- calls overwrote each other and writes were served from the wrong repo. MCP
-- revision 2026-07-28 removed protocol sessions outright and says servers
-- needing cross-call state must use explicit, server-minted handles passed as
-- ordinary tool arguments (SEP-2567). This table is that handle store.
--
-- No session_id column, and that absence is the design: a handle names what it
-- named when it was minted, for whoever holds it, on whatever connection.
-- Handles are NOT authentication — knomit has none, and anyone who can reach
-- the endpoint can already reach every repo by URL. A handle is unguessable
-- only so an agent cannot conjure one from a repo name it saw.
--
-- Durable across restarts (the stateless session-id manager accepts ids across
-- one, so a bridge that outlives a restart keeps its selection), but not
-- forever: last_used_at is stamped on every resolve and Store.Purge ages a
-- handle out after the same window a dead session gets
-- (session.client_retention, default 168h — a SETTING, not a constant).
--
-- session_bindings is deliberately NOT dropped here. Every control up-migration
-- is replayed as one concatenated script by `knomit migrate-registry` against a
-- home that may already carry these objects, so the chain admits idempotent
-- CREATE ... IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL). The
-- old table simply stops being read or written, and its rows carry no value.
-- NOTE for anyone editing this file: that guard splits on the semicolon BEFORE
-- it strips comments, so a semicolon in this prose reads as a statement break.
-- branch carries a per-handle read branch. '' means "the target's own read
-- branch", resolved exactly as NewBindingOfRepo(ri, "") does today -- the agent
-- branch for a normal repo, the followed upstream for a subscription. knomit_bind
-- does not accept a branch yet and always writes '', so today the column is
-- inert. It is here now so that per-handle branch switching, which is planned,
-- becomes a behaviour change rather than a migration: a column added later would
-- have to be added by ALTER TABLE, which this chain cannot express.
CREATE TABLE IF NOT EXISTS binding_handles (
    handle       TEXT PRIMARY KEY,
    binding      TEXT NOT NULL,
    branch       TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS binding_handles_last_used ON binding_handles(last_used_at);
