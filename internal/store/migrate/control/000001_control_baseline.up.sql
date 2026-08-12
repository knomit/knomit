-- control.db baseline: the shape three ad-hoc CREATE TABLE IF NOT EXISTS
-- callers built before control.db was versioned.
--
-- Every statement keeps IF NOT EXISTS deliberately. That makes this a no-op
-- against every home that already exists (which is then simply stamped v1),
-- and it satisfies the standing requirement that an up-migration body be safe
-- to re-run -- upWithRecovery may re-execute it after an interruption.
--
-- repos comes first: repo_origins, lenses and lens_reads all reference it.

CREATE TABLE IF NOT EXISTS repos (
    uid         TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    state       TEXT NOT NULL,
    profile     TEXT NOT NULL DEFAULT 'code',
    repo_id     TEXT,
    created_at  INTEGER NOT NULL,
    archived_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_name
    ON repos(name) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_repo_id
    ON repos(repo_id) WHERE state = 'active' AND repo_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS repo_origins (
    repo_uid    TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    branch      TEXT NOT NULL,
    auth_method TEXT NOT NULL DEFAULT '',
    auth_token  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS lenses (
    uid         TEXT PRIMARY KEY NOT NULL,
    name        TEXT NOT NULL,
    write_uid   TEXT NOT NULL REFERENCES repos(uid),
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS lenses_name ON lenses(name);

CREATE TABLE IF NOT EXISTS lens_reads (
    lens_uid  TEXT NOT NULL REFERENCES lenses(uid) ON DELETE CASCADE,
    repo_uid  TEXT NOT NULL REFERENCES repos(uid),
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_uid, repo_uid)
);
