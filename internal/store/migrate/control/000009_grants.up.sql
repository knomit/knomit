-- grants: what a verified principal may do on THIS instance (F19 phase 1).
-- Keyed by the principal's string form (<kind>:<id>@<via>, internal/auth
-- Principal.String), so a kernel-verified socket uid and a bearer-token
-- subject that happen to share an id never share a row -- the via is part
-- of the key.
--
-- Revocation is a timestamp, not a delete. The row is the audit trail, and
-- boot needs to tell "never granted" from "granted then revoked" so that
-- seeding the server's own socket uid never revives a permission the
-- operator took away.
--
-- This table is LOCAL policy. The master-signed fleet policy under
-- .knomit/policy/ (F19 phase 4) is merged at read time, never copied here.
--
-- Nothing here is derived from client_sessions, which stays an
-- observational trail that gates nothing (kb/invariants/mcp/client-sessions).
--
-- NOTE, the same one 000005 and 000007 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break and fails
-- TestControl_UpMigrationsAreIdempotentDDL. Do not put one here.
CREATE TABLE IF NOT EXISTS grants (
    principal   TEXT    NOT NULL,
    permission  TEXT    NOT NULL,
    granted_by  TEXT    NOT NULL DEFAULT '',
    granted_at  INTEGER NOT NULL,  -- Unix seconds
    revoked_at  INTEGER,           -- NULL means live
    PRIMARY KEY (principal, permission)
);
CREATE INDEX IF NOT EXISTS grants_principal_live ON grants(principal) WHERE revoked_at IS NULL;

-- client_session_peers: the pid the KERNEL reported for a unix-socket peer
-- (internal/auth.PeerCred), at most one row per session. ABSENCE means the
-- session arrived over TCP and has no peer -- there is no zero row to tell
-- apart from a missing one.
--
-- A SIDE TABLE and not a column on client_sessions because this chain is
-- replayed as one concatenated script by knomit migrate-registry and admits
-- idempotent CREATE IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL
-- enforces exactly that regex), so ALTER TABLE ADD COLUMN cannot be expressed
-- here at all. That is the 000007 precedent, and before it the
-- repo_subscriptions one.
--
-- The self-declared client_sessions.pid stays beside it ON PURPOSE. The
-- declared pid is a header the client wrote about itself and the verified one
-- is what the kernel said, so keeping both is what makes a mismatch visible
-- at all -- the same reason clientInfo is kept beside parent_app
-- (kb/decisions/mcp/client-sessions/identity-model).
--
-- This does NOT make client_sessions an authorization input. Nothing reads
-- this table to decide anything (kb/invariants/mcp/client-sessions) -- grants
-- is where permission lives, keyed by the principal, and this row is the
-- observational trail beside it.
--
-- No foreign key, purged alongside client_sessions in Store.Purge exactly as
-- client_session_bindings is.
--
-- NOTE, the same one 000005 and 000007 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS client_session_peers (
    session_id   TEXT PRIMARY KEY,
    verified_pid INTEGER NOT NULL,
    seen_at      INTEGER NOT NULL  -- Unix seconds
);
