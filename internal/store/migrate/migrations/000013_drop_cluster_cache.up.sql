-- The global Louvain cluster cache is gone: review clustering now runs Louvain
-- in-process over a bounded per-review subgraph (internal/synthesize), so there
-- is no precomputed per-(branch, resolution, min_community_size) result to
-- store. Drop the table; its rows were pure derived state.
DROP TABLE IF EXISTS cluster_cache;
