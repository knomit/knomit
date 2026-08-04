-- Drop the CHECK, restoring migration 4's table shape verbatim. No row is lost:
-- every state that satisfies the constraint also satisfies its absence.
CREATE TABLE repos_unchecked (
    name          TEXT NOT NULL,
    origin_url    TEXT NOT NULL DEFAULT '',
    origin_branch TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active',
    archive_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (name, archive_id)
);

INSERT INTO repos_unchecked (name, origin_url, origin_branch, state, archive_id, created_at, archived_at)
SELECT name, origin_url, origin_branch, state, archive_id, created_at, archived_at FROM repos;

DROP TABLE repos;
ALTER TABLE repos_unchecked RENAME TO repos;

CREATE INDEX IF NOT EXISTS idx_repos_state ON repos(state);
