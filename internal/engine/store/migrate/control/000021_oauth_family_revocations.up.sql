-- oauth_family_revocations: who revoked a token family from the operator's
-- list of approved logins (POST /api/v1/oauth/grants/{id}/revoke), written in
-- the transaction that sets the family's revoked_at. The first revoker is
-- kept, a second revoke of a revoked family records nothing. ABSENCE on a
-- revoked family means the server revoked it itself (a replayed refresh
-- token or code, or an approval that no longer stands) or it was revoked
-- before this migration.
--
-- A SIDE TABLE and not a column on oauth_families because this chain admits
-- idempotent CREATE IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL),
-- the 000011 and 000020 precedent.
--
-- The same NOTE as 000010: no semicolon anywhere in this prose.
CREATE TABLE IF NOT EXISTS oauth_family_revocations (
    family_id  TEXT PRIMARY KEY,
    revoked_by TEXT NOT NULL
);
