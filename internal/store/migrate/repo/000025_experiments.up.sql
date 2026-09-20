-- Experiments: a local `exp/<name>` branch forked from this instance's agent
-- branch, with the fork recorded EXPLICITLY.
--
-- The parent is a column, not something derived from the ref name, because a
-- branch holds a role only when an explicit record says so
-- (kb/invariants/store/branch-roles). Write eligibility asks this table
-- "is this experiment's recorded parent my current agent branch?"; inferring
-- the answer from the `exp/` prefix would make every foreign or stale
-- experiment writable the moment it appeared in the ref database.
--
-- NO FOREIGN KEY to branches(id), deliberately, and the key is the bare name
-- rather than a branch id. Foreign keys are enforced here (_foreign_keys=1),
-- and DropBranch deletes the branches row — so an FK would make rollback fail
-- at the last statement with the git ref already gone, which is exactly the
-- half-removed state DropBranch's ordering exists to avoid (it is also the bug
-- restatement_pairs had to be patched around in DropBranch). Rollback drops the
-- branch FIRST and deletes this row second, so a failure anywhere leaves the
-- operation retryable.
--
-- Local only: never pushed, never fetched, never advertised. This is repo-local
-- bookkeeping about a local branch, so it belongs in the repo DB and not in
-- git — the description in particular is display text, not knowledge.
--
-- No GraphSchemaVersion bump: this is authoritative local state, not derived
-- index state that a rebuild would leave stale (the 000018/000019/000023
-- precedent, and architecture/store/f6db3a49).
CREATE TABLE IF NOT EXISTS experiments (
    name             TEXT PRIMARY KEY,
    description      TEXT NOT NULL DEFAULT '',
    parent_branch    TEXT NOT NULL,
    fork_commit      TEXT NOT NULL,
    created_at       INTEGER NOT NULL,  -- Unix seconds
    last_activity_at INTEGER NOT NULL   -- Unix seconds
);

-- Eligibility asks "which experiments were forked from THIS agent branch?" on
-- every write gate that names an exp/* branch, and the sweeper scans by age.
CREATE INDEX IF NOT EXISTS experiments_parent   ON experiments(parent_branch);
CREATE INDEX IF NOT EXISTS experiments_activity ON experiments(last_activity_at);
