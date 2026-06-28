-- Recreate the cluster_cache table (matches migration 000004) so a rollback
-- restores the prior schema. It will be repopulated lazily if older code runs.
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
