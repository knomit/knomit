DROP INDEX IF EXISTS idx_ref_edges_target;
DELETE FROM meta WHERE key = 'ref_edges_backfilled';
DROP TABLE IF EXISTS ref_edges;
