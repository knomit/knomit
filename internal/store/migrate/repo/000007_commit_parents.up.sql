-- commit_parents records the parent edges of each indexed commit.
--
-- One row per (child, parent) edge. Multi-parent merges produce multiple
-- rows; parent_order preserves the canonical ordering — parent_order = 0 is
-- the "first parent" used for branch-local history walks (the "ours" side
-- of a merge), matching git's first-parent semantics.
--
-- Used by resolveActiveCommitForPath via a recursive CTE; replaces the
-- previous first_parent_chain virtual table whose Go-side cursor callback
-- re-entered the database/sql pool mid-scan and could deadlock under
-- concurrent load.
--
-- Populated inside the same transaction as commit_log + branch_commits in
-- Storer.CommitLogSync. Existing repos backfill from go-git on first open
-- via backfillCommitParents.
CREATE TABLE IF NOT EXISTS commit_parents (
    commit_hash  TEXT    NOT NULL,
    parent_order INTEGER NOT NULL,
    parent_hash  TEXT    NOT NULL,
    PRIMARY KEY (commit_hash, parent_order)
);
CREATE INDEX IF NOT EXISTS commit_parents_parent ON commit_parents (parent_hash);
