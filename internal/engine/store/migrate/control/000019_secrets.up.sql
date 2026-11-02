-- secrets: per-instance stored credentials (token, basic, ssh_key) that repos
-- link to and resolve at every git operation. Values and passphrases are
-- encrypted with the instance Crypt, the same one repo_origins.auth_token uses.
-- hosts is a JSON array of lowercased host names the secret may be sent to.
-- expires_at is advisory Unix seconds, NULL for none.
--
-- repo_origin_secrets: the link from a repo origin to its secret. NO ROW means
-- the repo uses its own pasted credential (or none). Deleting the origin
-- cascades to its link, and a secret cannot be deleted while a link names it
-- (ON DELETE RESTRICT).
--
-- A SEPARATE TABLE, not a secret_id column on repo_origins, because this chain
-- is replayed as one concatenated script and admits idempotent CREATE ... IF NOT
-- EXISTS only (TestControl_UpMigrationsAreIdempotentDDL). The repo_subscriptions
-- and handle_mounts precedent.
--
-- NOTE: the idempotency guard splits this file on the semicolon BEFORE it
-- strips comments, so a semicolon anywhere in this prose reads as a statement
-- break. Do not put one here.
CREATE TABLE IF NOT EXISTS secrets (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    type           TEXT NOT NULL,             -- token, basic or ssh_key
    hosts          TEXT NOT NULL,             -- JSON array of lowercased hosts
    value_enc      TEXT NOT NULL,
    passphrase_enc TEXT NOT NULL DEFAULT '',
    fingerprint    TEXT NOT NULL DEFAULT '',  -- SSH public key SHA256 fingerprint
    expires_at     INTEGER,                   -- Unix seconds, NULL = none
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS secrets_name ON secrets(name);
CREATE TABLE IF NOT EXISTS repo_origin_secrets (
    repo_uid  TEXT PRIMARY KEY REFERENCES repo_origins(repo_uid) ON DELETE CASCADE,
    secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS repo_origin_secrets_secret ON repo_origin_secrets(secret_id);
