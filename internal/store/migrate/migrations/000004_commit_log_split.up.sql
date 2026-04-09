-- commit_log becomes branch-agnostic: one row per (commit_hash, path).
-- Branch visibility is tracked separately in branch_commits.
-- This is a breaking schema change. Any existing data in commit_log is dropped;
-- it will be repopulated by populateCommitLog on next boot from git history.
DROP INDEX IF EXISTS commit_log_branch;
DROP TABLE IF EXISTS commit_log;

CREATE TABLE commit_log (
    commit_hash  TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    committed_at INTEGER NOT NULL,
    message      TEXT    NOT NULL,
    operation    TEXT    NOT NULL DEFAULT '',
    author_email TEXT    NOT NULL DEFAULT '',
    action       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (commit_hash, path)
);
CREATE INDEX commit_log_path_time ON commit_log (path, committed_at DESC);
CREATE INDEX commit_log_time      ON commit_log (committed_at DESC);
CREATE INDEX commit_log_operation ON commit_log (operation, committed_at DESC);

-- Many-to-many: which branches see which commits.
-- A commit is on a branch iff it is reachable by walking parents from the branch ref.
-- Populated by populateCommitLog (git walk) and CreateBranch (clone from parent).
-- ON DELETE CASCADE: dropping a branch row also drops its visibility rows.
CREATE TABLE branch_commits (
    branch_id   INTEGER NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    commit_hash TEXT    NOT NULL,
    PRIMARY KEY (branch_id, commit_hash)
);
CREATE INDEX branch_commits_hash ON branch_commits (commit_hash);
