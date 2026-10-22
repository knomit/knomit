-- Key the index schema version per branch, mirroring last_commit:<branch>.
--
-- Rebuild used to write ONE global meta.graph_schema_version row, but the work
-- it records is per-branch: any branch's success marked the whole DB current
-- and masked every other branch's failure. The workaround for that masking
-- (IndexManager.MarkRebuildNeeded, which DELETED the row whenever a multi-branch
-- heal was partial) turned a permanent single-branch failure into an unbounded
-- full-repo re-index — the failing branch re-armed the global retry, the healthy
-- branch rebuilt from scratch again, forever, while the repo still reported a
-- healthy index. A branch whose stored upstream names a ref that does not exist
-- locally reaches exactly that state and never leaves it.
--
-- Per-branch keys make the retry proportional: the branch that failed stays
-- stale, the branches that succeeded stay done, and MarkRebuildNeeded is gone.
--
-- Carry the existing global value onto every branch the index already knows
-- about, so upgrading a DB that is already current does not force a re-index. A
-- branch absent from `branches` has never been indexed; it gets no row and so
-- reads as stale, which is what a never-indexed branch needs anyway (its
-- Rebuild and its first Sync do the same full pass). Then drop the global key so
-- no reader can fall back to it.
INSERT OR IGNORE INTO meta(key, value)
SELECT 'graph_schema_version:' || b.name, m.value
  FROM branches b
  JOIN meta m ON m.key = 'graph_schema_version';

DELETE FROM meta WHERE key = 'graph_schema_version';
