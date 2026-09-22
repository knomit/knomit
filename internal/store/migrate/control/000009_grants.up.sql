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
