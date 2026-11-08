-- oauth_family_clients: the client_name of the request a token family came
-- from, carried over from the pending row in the same transaction that
-- creates the family when the browser collects the code. The operator's
-- list of approved logins (GET /api/v1/oauth/grants) shows it, so a login
-- reads as "Claude Code" rather than its client id URL. The name is what the
-- CLIENT said about itself, never an identity. ABSENCE means a family minted
-- before this migration, or a request that gave no name, and the list then
-- shows the client id.
--
-- Who approved the family is already in oauth_family_approvals (000011).
--
-- A SIDE TABLE and not a column on oauth_families because this chain admits
-- idempotent CREATE IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL),
-- the 000011 precedent.
--
-- The same NOTE as 000010: no semicolon anywhere in this prose.
CREATE TABLE IF NOT EXISTS oauth_family_clients (
    family_id   TEXT PRIMARY KEY,
    client_name TEXT NOT NULL
);
