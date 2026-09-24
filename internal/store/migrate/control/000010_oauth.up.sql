-- OAuth issuer state (F19 phase 3a): the pending-authorization store, the
-- token families, the single-use authorization codes and the tokens.
--
-- NO TOKEN AND NO CODE IS STORED. Tokens and codes are 32 random bytes, and
-- the hash column is the hex SHA-256 of the string the client holds, so a
-- copied control.db yields nothing a client could present.
--
-- A FAMILY is one login: one client, one subject, one ceiling, one audience.
-- It is the unit of revocation -- revoking any token of a family, presenting a
-- rotated refresh token, or replaying a consumed code sets revoked_at here,
-- and every token and code in the family is dead from that moment.
-- refresh_expires_at is ABSOLUTE from the family's first issuance, never
-- extended by a refresh.
--
-- resource is stored EXACTLY as the client named it (a URL under the issuer)
-- and a request is in audience when its canonical URL sits under it at a /
-- boundary. subject is the name the approving operator gave, and the
-- principal is host:<subject>@token.
--
-- A pending row is created by /oauth/authorize and decided by an operator
-- over the local listener. The code is minted when the browser COLLECTS the
-- decision on /wait, not at approval, which is what lets the code be stored
-- as a hash only: nobody else ever needs the plaintext. collected_at makes
-- that a one-time event.
--
-- Nothing here authenticates anything by itself. The token principal is
-- recomputed from the Authorization header on every request.
--
-- NOTE, the same one 000005, 000007 and 000009 carry: the idempotency guard
-- splits this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS oauth_pending (
    id             TEXT PRIMARY KEY,
    client_id      TEXT    NOT NULL,
    client_name    TEXT    NOT NULL DEFAULT '',
    redirect_uri   TEXT    NOT NULL,
    scope          TEXT    NOT NULL DEFAULT '',  -- as requested, space-separated
    code_challenge TEXT    NOT NULL,
    resource       TEXT    NOT NULL,
    state          TEXT    NOT NULL DEFAULT '',
    remote_addr    TEXT    NOT NULL DEFAULT '',
    user_agent     TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,             -- Unix seconds
    expires_at     INTEGER NOT NULL,
    decision       TEXT    NOT NULL DEFAULT '',  -- '' | approved | denied
    subject        TEXT    NOT NULL DEFAULT '',
    ceiling        TEXT    NOT NULL DEFAULT '',  -- granted, space-separated
    decided_by     TEXT    NOT NULL DEFAULT '',
    decided_at     INTEGER,
    collected_at   INTEGER
);
CREATE INDEX IF NOT EXISTS oauth_pending_live ON oauth_pending(expires_at) WHERE decision = '';

CREATE TABLE IF NOT EXISTS oauth_families (
    id                 TEXT PRIMARY KEY,
    client_id          TEXT    NOT NULL,
    subject            TEXT    NOT NULL,
    scope              TEXT    NOT NULL,  -- the ceiling, space-separated
    resource           TEXT    NOT NULL,
    created_at         INTEGER NOT NULL,
    refresh_expires_at INTEGER NOT NULL,
    revoked_at         INTEGER
);

CREATE TABLE IF NOT EXISTS oauth_codes (
    hash           TEXT PRIMARY KEY,
    family         TEXT    NOT NULL,
    client_id      TEXT    NOT NULL,
    redirect_uri   TEXT    NOT NULL,
    code_challenge TEXT    NOT NULL,
    resource       TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    consumed_at    INTEGER
);
CREATE INDEX IF NOT EXISTS oauth_codes_family ON oauth_codes(family);

CREATE TABLE IF NOT EXISTS oauth_tokens (
    hash       TEXT PRIMARY KEY,
    kind       TEXT    NOT NULL,  -- access | refresh
    family     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    rotated_at INTEGER            -- refresh only: set when exchanged
);
CREATE INDEX IF NOT EXISTS oauth_tokens_family ON oauth_tokens(family);
