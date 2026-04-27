-- cluster_cache stores precomputed Louvain community detection results so that
-- knomit_review (which clusters all dirty facts) does not recompute them on
-- every call. One row per (branch, resolution, min_community_size). The cache
-- is invalidated by comparing head_commit against the branch's current HEAD;
-- the background checker in internal/clustercache refreshes stale rows during
-- quiet periods.
CREATE TABLE IF NOT EXISTS cluster_cache (
    branch_id          INTEGER NOT NULL,
    resolution         REAL    NOT NULL,
    min_community_size INTEGER NOT NULL,
    head_commit        TEXT    NOT NULL,
    clusters_json      TEXT    NOT NULL,
    noise_json         TEXT    NOT NULL,
    computed_at        INTEGER NOT NULL,
    PRIMARY KEY (branch_id, resolution, min_community_size),
    FOREIGN KEY (branch_id) REFERENCES branches(id) ON DELETE CASCADE
);
