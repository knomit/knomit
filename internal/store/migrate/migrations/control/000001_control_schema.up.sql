-- Folds in the previously ad-hoc control.db DDL. Deliberately IF NOT EXISTS:
-- installs created before control.db was versioned already have these tables
-- and no schema_migrations row, so migration 1 must adopt them, not fail.
--
-- This is the schema as it existed before the `description` column (added in
-- migration 2 below), so it matches genuinely old installs character for
-- character — see internal/repos/lens.go and internal/repos/settings.go.
CREATE TABLE IF NOT EXISTS lenses (
    name       TEXT PRIMARY KEY,
    write_repo TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);

CREATE TABLE IF NOT EXISTS repo_settings (
    repo_id TEXT PRIMARY KEY,
    profile TEXT NOT NULL
);
