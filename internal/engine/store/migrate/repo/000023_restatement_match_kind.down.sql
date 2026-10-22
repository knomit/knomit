-- Symmetric rebuild back to the 000018 shape, and idempotent for the same
-- reason the up is: a cache with nothing to preserve.
DROP TABLE IF EXISTS restatement_pairs;

CREATE TABLE restatement_pairs (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    a_path     TEXT NOT NULL,
    b_path     TEXT NOT NULL,
    a_fact_id  INTEGER NOT NULL,
    b_fact_id  INTEGER NOT NULL,
    title_cos  REAL NOT NULL,
    PRIMARY KEY (branch_id, a_path, b_path)
);
CREATE INDEX restatement_pairs_rank
    ON restatement_pairs(branch_id, title_cos DESC);
CREATE INDEX restatement_pairs_a ON restatement_pairs(branch_id, a_fact_id);
CREATE INDEX restatement_pairs_b ON restatement_pairs(branch_id, b_fact_id);

DELETE FROM restatement_cache_state;
