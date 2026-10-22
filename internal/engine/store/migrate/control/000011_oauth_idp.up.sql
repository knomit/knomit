-- OAuth consent path 3, an external identity provider (F19 phase 3c).
--
-- oauth_pending_idp: the hex SHA-256 of the browser-binding cookie that
-- /oauth/authorize set for a request when [oauth.idp] was configured. The
-- provider sign-in for that request (start, callback and the decide POST)
-- is honoured only in the browser holding the cookie, so a forwarded
-- start link fails in anyone else's browser. ABSENCE means the request was
-- parked with no provider configured and cannot be approved by path 3. The
-- cookie itself is never stored. Rows go with their request when expired
-- requests are purged.
--
-- oauth_family_approvals: who approved the request a token family came
-- from -- the pending row's decided_by, carried over when the browser
-- collects the code. A family approved by path 3 (idp:github:<id>) is
-- refused at refresh, and revoked, once that id is no longer on the allow
-- list. ABSENCE means a family minted before this migration, which is
-- treated as approved by an operator.
--
-- SIDE TABLES and not columns because this chain admits idempotent CREATE
-- IF NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL), the
-- 000007 and 000009 precedent.
--
-- The same NOTE as 000010: no semicolon anywhere in this prose.
CREATE TABLE IF NOT EXISTS oauth_pending_idp (
    pending_id TEXT PRIMARY KEY,
    binding    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS oauth_family_approvals (
    family_id   TEXT PRIMARY KEY,
    approved_by TEXT NOT NULL
);
