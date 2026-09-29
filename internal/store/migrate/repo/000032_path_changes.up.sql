-- Precomputed per-path change history for knomit_explain (see
-- internal/store/path_changes.go and
-- kb/decisions/mcp/explain/history-enumeration). DERIVED STATE, regenerated from
-- git: the tables start empty and syncPathChanges fills them for every indexed
-- commit not yet in path_change_commits (on repo open, populate and append).
--
-- Every row is CONTENT-ADDRESSED: it is keyed by commit and derived only from
-- git objects (the commit, its parents' trees, and the rows of its ancestors),
-- which never change for a given hash. So no branch rewind, purge or commit_log
-- rebuild can make a row stale, and none of them touch these tables. A change
-- to the derivation itself bumps pathChangesVersion, which empties the three
-- tables and recomputes them.

-- One row per (path, commit) where the path's blob at the commit differs from
-- its blob at the FIRST parent (exactly the commit_log add/modify rows, for
-- .md paths):
--   entry = 1  the commit introduced the blob: it differs from EVERY parent.
--              resolves_to = commit_hash.
--   entry = 0  a CARRY: a merge whose blob equals a non-first parent's (a PR
--              merge). resolves_to = the entry that introduced that content.
-- order_at/gen are the sort key (entries only): order_at = max(author_at, the
-- order_at of every change it was edited from), gen = 1 + their max gen.
-- diff is the entry's store.RevisionDiff against the content it was edited
-- from (its lowest-ordered link's from_blob), as JSON; '' for none.
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
    PRIMARY KEY (path, commit_hash)
);
CREATE INDEX IF NOT EXISTS path_changes_entry_blob ON path_changes (path, blob) WHERE entry = 1;

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

-- Commits whose path_changes rows have been derived (possibly none).
CREATE TABLE IF NOT EXISTS path_change_commits (
    commit_hash TEXT PRIMARY KEY
);
