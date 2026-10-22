-- mount_experiments: which experiment a session is working inside ON A
-- URL-SCOPED MOUNT — /repos/{repo}/branches/{branch}/mcp and
-- /lenses/{lens}/mcp.
--
-- THIS IS SESSION-KEYED STATE, which 000007 above rejects in as many words.
-- The difference is the blast radius, and it is the whole argument, so read it
-- before adding anything here.
--
-- On the UNSCOPED mount nothing identifies the caller except the handle it was
-- given, so session-keyed routing there decides WHICH KNOWLEDGE BASE a write
-- lands in. That is the 2026-09-17 incident: five writes served from the wrong
-- repo. binding_handles and handle_experiments exist for that mount and that
-- reason, and this table does not touch it.
--
-- A URL-scoped mount already names its repo or lens IN THE URL. The mount uid
-- in this key is that identity, so the only thing this state can decide is
-- WHICH BRANCH WITHIN THAT ONE REPO — the agent branch or an experiment forked
-- from it. It cannot reach another knowledge base, because the URL got there
-- first and this table is never consulted to choose one.
--
-- WHAT IS ACCEPTED BY BUILDING THIS. A client that shares one connection
-- across several logical jobs shares one Mcp-Session-Id, so two jobs on the
-- SAME mount share a row: one job opening an experiment moves the other onto
-- it. Both branches share an ontology by construction, so such a write
-- succeeds rather than failing on an unknown topic. That is a real narrowing
-- of attribution and it was accepted deliberately — the alternative was
-- requiring a reconnect to enter an experiment, which made the feature
-- unusable on the bridges most people run. Nothing in the wire distinguishes
-- two jobs on one connection, so no key can separate them here.
--
-- KEYED ON (session id, mount uid), never on session id alone. One session may
-- hold several URL-scoped mounts at once — a repo bridge and a lens bridge in
-- the same client — and each carries its own experiment or none.
--
-- No foreign key, following client_session_bindings and handle_experiments.
-- The rows are purged alongside client_sessions in Store.Purge, which keeps
-- the delete order from mattering.
--
-- Absence means "no experiment" and there is no empty-string row to tell apart
-- from a missing one.
--
-- NOTE, the same one 000005 and 000007 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break and fails
-- TestControl_UpMigrationsAreIdempotentDDL. Do not put one here.
CREATE TABLE IF NOT EXISTS mount_experiments (
    session_id TEXT NOT NULL,
    mount_uid  TEXT NOT NULL,
    experiment TEXT NOT NULL,
    set_at     INTEGER NOT NULL,  -- Unix seconds
    PRIMARY KEY (session_id, mount_uid)
);
