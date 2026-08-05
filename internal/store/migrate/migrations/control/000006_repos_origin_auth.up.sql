-- Origin credentials move into the control plane.
--
-- A repo whose database is lost is re-cloned at boot from its recorded origin,
-- but the credential needed to reach a private origin lived encrypted inside
-- that same database. The registry promised a recovery it could not perform.
--
-- Additive ALTERs rather than a table rebuild: migrations 4 and 5 already
-- rebuilt this table, and re-copying rows a third time buys nothing. The
-- columns default to '' so every existing row is valid the moment they appear.
--
-- auth_token holds AES-256-GCM ciphertext keyed from the agent SSH key, never
-- plaintext. It is written ONLY by RepoRegistry.SetOriginCredential; the
-- ordinary row upsert deliberately leaves both columns alone.
ALTER TABLE repos ADD COLUMN auth_method TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN auth_token  TEXT NOT NULL DEFAULT '';
