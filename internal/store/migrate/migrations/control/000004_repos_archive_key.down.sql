-- Narrow the primary key back to (name). Rows that collide on name are lost;
-- keep the active one when there is a choice, since that is the row a running
-- server reads at startup.
CREATE TABLE repos_old (
    name          TEXT PRIMARY KEY,
    origin_url    TEXT NOT NULL DEFAULT '',
    origin_branch TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active',
    archive_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0
);

INSERT OR REPLACE INTO repos_old (name, origin_url, origin_branch, state, archive_id, created_at, archived_at)
SELECT name, origin_url, origin_branch, state, archive_id, created_at, archived_at
FROM repos ORDER BY (state = 'active') ASC;

DROP TABLE repos;
ALTER TABLE repos_old RENAME TO repos;

CREATE INDEX IF NOT EXISTS idx_repos_state ON repos(state);
