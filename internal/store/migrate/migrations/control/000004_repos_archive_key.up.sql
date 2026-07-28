-- Widen the repos primary key from (name) to (name, archive_id).
--
-- A repo name is unique only among ACTIVE repos. Archiving "work" and then
-- creating a new "work" is supported (and tested) behaviour, as is archiving
-- the same name twice — so with name alone as the key, the new active row
-- overwrote the archived one and the archived repo became unrecoverable the
-- moment its name was reused. That was tolerable while the archive record
-- lived in repos/archive/<id>.json (keyed by the id); it is not once the
-- registry is the only place that record exists.
--
-- Active rows keep archive_id = '', so a name still has at most one active
-- row and upserting an active repo by name still updates in place.
CREATE TABLE repos_new (
    name          TEXT NOT NULL,
    origin_url    TEXT NOT NULL DEFAULT '',
    origin_branch TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active',
    archive_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (name, archive_id)
);

INSERT INTO repos_new (name, origin_url, origin_branch, state, archive_id, created_at, archived_at)
SELECT name, origin_url, origin_branch, state, archive_id, created_at, archived_at FROM repos;

DROP TABLE repos;
ALTER TABLE repos_new RENAME TO repos;

CREATE INDEX IF NOT EXISTS idx_repos_state ON repos(state);
