-- Precomputed per-path change history for knomit_explain (see
-- internal/store/path_changes.go and
-- kb/decisions/mcp/explain/history-enumeration). DERIVED STATE, computed from
-- git objects alone and IMMUTABLE per commit hash: nothing deletes these rows
-- on a rewind or a rebuild. A commit is derived before the ref that makes it
-- visible advances, and storegit.CommitLogApply's hook refuses to record one
-- that is not, so an indexed commit is never visible without its rows.
-- There is deliberately no path_changes_version row: its absence makes the
-- next open derive every branch's history in one transaction before the repo
-- serves anything (openHistory); a version change does the same.

-- One row per (path, commit) where the path's blob at the commit differs from
-- its blob at the FIRST parent (the tree diff's add/modify rows, for .md
-- paths):
--   entry = 1  the commit introduced the blob: it differs from EVERY parent.
--              resolves_to = commit_hash.
--   entry = 0  a CARRY: the blob equals a non-first parent's (a PR merge), or
--              the first parent's (a case-only rename). resolves_to = the
--              entry that introduced that content.
-- order_at/gen are the sort key (entries only). diff is the RevisionDiff JSON
-- against the content the change was edited from. fp_depth is the commit's
-- first-parent depth. content_unavailable: the content or its diff base is
-- missing from the object store (blob '' when unknown; no diff).
CREATE TABLE IF NOT EXISTS path_changes (
    path        TEXT    NOT NULL,
    commit_hash TEXT    NOT NULL,
    entry       INTEGER NOT NULL,
    resolves_to TEXT    NOT NULL,
    blob        TEXT    NOT NULL,
    action      TEXT    NOT NULL DEFAULT '',
    author_at   INTEGER NOT NULL DEFAULT 0,
    order_at    INTEGER NOT NULL DEFAULT 0,
    gen         INTEGER NOT NULL DEFAULT 0,
    message     TEXT    NOT NULL DEFAULT '',
    diff        TEXT    NOT NULL DEFAULT '',
    fp_depth    INTEGER NOT NULL DEFAULT 0,
    content_unavailable INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (path, commit_hash)
);
CREATE INDEX IF NOT EXISTS path_changes_depth ON path_changes (path, fp_depth);

-- The edited-from links of an entry, one per parent that has the path: the
-- entry live at that parent (from_commit; '' when unresolvable) and the
-- parent's blob (from_blob; '' when unknown).
CREATE TABLE IF NOT EXISTS path_change_links (
    path         TEXT    NOT NULL,
    commit_hash  TEXT    NOT NULL,
    parent_order INTEGER NOT NULL,
    from_commit  TEXT    NOT NULL,
    from_blob    TEXT    NOT NULL,
    PRIMARY KEY (path, commit_hash, parent_order)
);

-- Every derived commit: its first-parent depth (0 for a root or below a
-- boundary), its first-parent JUMP POINTERS (ups: 20-byte raw hashes, entry k
-- 2^k first-parent steps below), and degraded (0 none; 1 a revision is
-- content_unavailable; 2 a missing parent commit here or below cut the depth)
-- — what :rebuild re-derives once objects are back. Presence is the "derived"
-- mark.
CREATE TABLE IF NOT EXISTS commit_fp (
    commit_hash TEXT    PRIMARY KEY,
    depth       INTEGER NOT NULL,
    ups         BLOB    NOT NULL,
    degraded    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS commit_fp_degraded ON commit_fp (degraded) WHERE degraded > 0;

-- :rebuild's staged commit_log rewrite (rebuildCommitLog): written outside the
-- write lock, then swapped in with bulk statements in one short transaction.
CREATE TABLE IF NOT EXISTS commit_log_stage (
    branch_id    INTEGER NOT NULL,
    commit_hash  TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    message      TEXT    NOT NULL,
    operation    TEXT    NOT NULL DEFAULT '',
    author_name  TEXT    NOT NULL DEFAULT '',
    author_email TEXT    NOT NULL DEFAULT '',
    action       TEXT    NOT NULL DEFAULT '',
    committed_at INTEGER NOT NULL,
    PRIMARY KEY (branch_id, commit_hash, path)
);
