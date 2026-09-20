-- handle_experiments: which experiment, if any, a binding handle is working
-- inside. Routing state, like binding_handles itself — it decides which branch
-- a write lands on — and therefore deliberately NOT in client_sessions, where
-- nothing may gate.
--
-- KEYED ON THE HANDLE, never on Mcp-Session-Id. A client may share one
-- connection, and so one session id, across several logical jobs, so
-- session-keyed state serves whichever job acted last: that is the 2026-09-17
-- cross-binding incident, in which five writes were served from the wrong
-- knowledge base and failed only because the two ontologies happened to share
-- no topic. An active experiment has the same shape and a worse failure — the
-- experiment and its parent share an ontology by construction, so a misrouted
-- write would commit silently. The handle is the only value that distinguishes
-- two callers on one connection.
--
-- A SEPARATE TABLE, not a column on binding_handles, because this chain is
-- replayed as one concatenated script by `knomit migrate-registry` and admits
-- idempotent CREATE ... IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL
-- enforces exactly that regex). ALTER TABLE ADD COLUMN cannot be expressed
-- here. This is the repo_subscriptions precedent: a presence table standing in
-- for a column the chain cannot add.
--
-- No foreign key to binding_handles, following client_session_bindings: the
-- orphans are collected in Store.Purge alongside the handles themselves, which
-- keeps the delete order from mattering and avoids a cascade this chain would
-- have no way to alter later.
--
-- One row per handle at most, so the handle is the primary key: a handle is
-- inside one experiment or none. Absence means none; there is no empty-string
-- row to distinguish from a missing one.
CREATE TABLE IF NOT EXISTS handle_experiments (
    handle     TEXT PRIMARY KEY,
    experiment TEXT NOT NULL,
    set_at     INTEGER NOT NULL  -- Unix seconds
);
