-- Restores the column shape only. The connection identity itself is NOT
-- recoverable from this database — it lives in <home>/control.db (repo_origins)
-- — so a rolled-back database comes back with empty connection columns and the
-- pre-Task-17 reader would report "no origin" until control.db re-populates it.
ALTER TABLE remotes ADD COLUMN url              TEXT NOT NULL DEFAULT '';
ALTER TABLE remotes ADD COLUMN branch           TEXT NOT NULL DEFAULT 'main';
ALTER TABLE remotes ADD COLUMN auth_method      TEXT NOT NULL DEFAULT '';
ALTER TABLE remotes ADD COLUMN auth_token       TEXT NOT NULL DEFAULT '';
