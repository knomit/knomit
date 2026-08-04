-- Constrain repos.state to the two values the code actually knows about.
--
-- A row holding anything else is invisible to BOTH List(RepoActive) and
-- List(RepoArchived), because each filters on an exact match. That repo and its
-- database would then be missing from every surface — the API, the boot
-- reconcile, ListArchived — with nothing logged anywhere, which is the one
-- failure mode this registry exists to eliminate. Everything else here is loud;
-- an out-of-range state was the remaining way to disappear quietly.
--
-- SQLite cannot add a CHECK to an existing table, so the table is rebuilt, the
-- same way migration 4 widened the primary key.
--
-- A row that ALREADY holds a bad state is repaired rather than rejected.
-- archive_id says unambiguously which state the row meant — active rows carry
-- '' and archived rows carry their id — and failing the migration instead would
-- take the whole instance offline over one unreadable row, which is exactly the
-- trade the boot reconcile refuses to make elsewhere.
UPDATE repos SET state = CASE WHEN archive_id = '' THEN 'active' ELSE 'archived' END
WHERE state NOT IN ('active', 'archived');

CREATE TABLE repos_checked (
    name          TEXT NOT NULL,
    origin_url    TEXT NOT NULL DEFAULT '',
    origin_branch TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'archived')),
    archive_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (name, archive_id)
);

INSERT INTO repos_checked (name, origin_url, origin_branch, state, archive_id, created_at, archived_at)
SELECT name, origin_url, origin_branch, state, archive_id, created_at, archived_at FROM repos;

DROP TABLE repos;
ALTER TABLE repos_checked RENAME TO repos;

CREATE INDEX IF NOT EXISTS idx_repos_state ON repos(state);
