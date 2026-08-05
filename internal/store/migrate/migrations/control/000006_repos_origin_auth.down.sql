-- SQLite supports DROP COLUMN from 3.35; the driver bundled here is newer.
-- Dropping loses the only recoverable copy of every credential, which is the
-- point of the up migration — so a down migration is a deliberate downgrade.
ALTER TABLE repos DROP COLUMN auth_token;
ALTER TABLE repos DROP COLUMN auth_method;
