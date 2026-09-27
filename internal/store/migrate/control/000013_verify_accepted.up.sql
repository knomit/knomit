-- F09: the operator's accept list for commit signature verification, one per
-- INSTANCE (user ruling, PR 4 option a).
--
-- `knomit verify accept <commit>` records a waiver here. Its only reader is
-- E4, the clone-time check of origin's copy of this instance's own agent
-- branch: an accepted commit there is adopted even though this instance did
-- not sign it. The acceptance gate (knomit verify ci) never reads it.
--
-- Here and not in each repository's database because the clone-time E4 check
-- refuses DURING a create, when the repository's database is brand new and a
-- failed create discards it. An instance-level list exists before any clone,
-- and survives SwapStore and re-create. The repo-database table of the same
-- name (repo migration 000027) is never written and never read.
--
-- repo_uid NULL means the waiver applies to that commit in ANY repository.
-- Commit hashes are content-addressed, so a waiver by hash is precise.
--
-- NOTE, the same one 000005 and 000007 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS verify_accepted (
    commit_hash TEXT PRIMARY KEY,
    repo_uid    TEXT,
    note        TEXT NOT NULL DEFAULT '',
    accepted_at INTEGER NOT NULL
);
