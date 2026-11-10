-- Credentials of the unified auth model (plan P3): browser sessions,
-- one-time login links, personal access tokens and browser pairings. They
-- are built DARK: until the flip (P4) nothing on the main port reads them.
--
-- NO SECRET IS STORED. A session cookie, a login link token and a personal
-- access token are random strings the client holds, and every hash column is
-- the hex SHA-256 of that string (oauth.hashSecret), so a copied control.db
-- yields nothing a client could present.
--
-- auth_sessions is one browser session. principal is the authz principal it
-- acts as (authz.Principal.String). A session is dead once revoked_at is set,
-- once now passes absolute_expires_at, or once it went unused for the idle
-- lifetime since last_seen_at. Times are Unix seconds.
--
-- auth_login_links is one single-use login link (knomit auth link). used_at is
-- set by the one UPDATE that consumes it, guarded by used_at IS NULL.
--
-- auth_tokens is one personal access token. Its principal is token:<id> and
-- its one role binding lives in auth_role_bindings (000022). role here is a
-- copy for listing only. There is no issuer column: a token is valid on every
-- external address (design 15.2).
--
-- auth_pairing is the browser half of a pairing request whose queue row is
-- in oauth_pending (client knomit-web). code is the short code a person types.
-- session_hash is the hash of the session minted when the waiting browser
-- collected the approval.
--
-- NOTE: the idempotency guard splits this file on the semicolon BEFORE it
-- strips comments, so a semicolon anywhere in this prose reads as a statement
-- break. Do not put one here.
CREATE TABLE IF NOT EXISTS auth_sessions (
    id                  TEXT PRIMARY KEY,
    hash                TEXT    NOT NULL UNIQUE,
    principal           TEXT    NOT NULL,
    user_agent          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    revoked_at          INTEGER
);

CREATE TABLE IF NOT EXISTS auth_login_links (
    hash       TEXT PRIMARY KEY,
    principal  TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);

CREATE TABLE IF NOT EXISTS auth_tokens (
    id           TEXT PRIMARY KEY,
    hash         TEXT    NOT NULL UNIQUE,
    label        TEXT    NOT NULL DEFAULT '',
    role         TEXT    NOT NULL,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);

CREATE TABLE IF NOT EXISTS auth_pairing (
    pending_id   TEXT PRIMARY KEY,
    code         TEXT NOT NULL UNIQUE,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    session_hash TEXT NOT NULL DEFAULT ''
);
