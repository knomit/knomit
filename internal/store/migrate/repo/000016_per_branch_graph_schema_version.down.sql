-- Collapse the per-branch schema versions back to one global row.
--
-- Only when every branch agrees: the global key cannot represent "branch A is
-- current, branch B is stale", and picking either value would be a lie in one
-- direction. Restoring nothing is the safe lie — a missing row reads as stale,
-- so the pre-migration code rebuilds every branch, which is correct if wasteful.
-- HAVING with no GROUP BY filters the single aggregate row, so a disagreeing (or
-- empty) set inserts nothing.
--
-- GLOB, not LIKE: `_` is a single-character wildcard in LIKE, so the pattern
-- would also match keys that merely resemble 'graph_schema_version:'.
INSERT OR REPLACE INTO meta(key, value)
SELECT 'graph_schema_version', MIN(value)
  FROM meta
 WHERE key GLOB 'graph_schema_version:*'
HAVING COUNT(DISTINCT value) = 1;

DELETE FROM meta WHERE key GLOB 'graph_schema_version:*';
