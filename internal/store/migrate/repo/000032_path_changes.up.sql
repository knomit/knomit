-- Precomputed per-path change history for knomit_explain (see
-- internal/store/path_changes.go and
-- kb/decisions/mcp/explain/history-enumeration). DERIVED STATE, computed from
-- git objects alone and IMMUTABLE per commit hash: nothing deletes these rows
-- on a rewind or a rebuild; a pathChangesVersion bump is the only reset.
-- A commit's rows are committed no later than the transaction that records it
-- in branch_commits (storegit.CommitLogApply's Derive hook re-checks), so an
-- indexed commit is never visible without them. A database that predates this
-- migration is derived for every branch tip on its first populate
-- (ensureAllDerived), while the repo opens.

-- One row per (path, commit) where the path's blob at the commit differs from
-- its blob at the FIRST parent (the commit_log add/modify rows, for .md paths):
--   entry = 1  the commit introduced the blob: it differs from EVERY parent.
--              resolves_to = commit_hash.
--   entry = 0  a CARRY: a merge whose blob equals a non-first parent's (a PR
--              merge). resolves_to = the entry that introduced that content.
-- order_at/gen are the sort key (entries only): order_at = max(author_at, the
-- order_at of every change it was edited from), gen = 1 + their max gen.
-- diff is the entry's store.RevisionDiff against the content it was edited
-- from (its first link's from_blob), as JSON; '' for none. fp_depth is the
-- commit's first-parent depth (commit_fp), which the live-change lookup uses.
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
    PRIMARY KEY (path, commit_hash)
);
CREATE INDEX IF NOT EXISTS path_changes_depth ON path_changes (path, fp_depth);

-- The edited-from links of an entry, one per parent that has the path: the
-- entry live at that parent (from_commit; '' when unresolvable) and the
-- parent's blob (from_blob). parent_order 0 is the first parent. A revision is
-- diffed against the lowest-ordered link's from_blob.
CREATE TABLE IF NOT EXISTS path_change_links (
    path         TEXT    NOT NULL,
    commit_hash  TEXT    NOT NULL,
    parent_order INTEGER NOT NULL,
    from_commit  TEXT    NOT NULL,
    from_blob    TEXT    NOT NULL,
    PRIMARY KEY (path, commit_hash, parent_order)
);

-- Every derived commit: its first-parent depth (0 for a root) and its
-- first-parent JUMP POINTERS (binary lifting): ups is the concatenation of
-- 20-byte raw hashes, entry k being the commit 2^k first-parent steps below.
-- "Is R on X's first-parent line" then costs O(log depth) lookups instead of
-- a walk. One row per commit keeps the per-write footprint to one row.
-- Presence is the "derived" mark.
CREATE TABLE IF NOT EXISTS commit_fp (
    commit_hash TEXT    PRIMARY KEY,
    depth       INTEGER NOT NULL,
    ups         BLOB    NOT NULL
);
-- No path_changes_version row: its absence makes the first populate derive
-- every branch tip's history (ensureAllDerived), then write the version.
