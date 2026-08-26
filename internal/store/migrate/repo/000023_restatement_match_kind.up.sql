-- How a standing restatement pair was FOUND, so selection can tell the two
-- detection routes apart (#127).
--
-- The shortlist has ranked candidates by title cosine alone since it existed.
-- That is the right ranking for a pair discovered BY title cosine, and the
-- wrong one for a pair discovered structurally — two facts that share a
-- normalised path identity, or a token that is rare in this corpus, are
-- near-certain duplicates whatever their titles happen to score. Ranking those
-- by cosine would leave them standing in the cache and never selected: real
-- detection that never reaches the judge, which is the exact failure this issue
-- is about.
--
-- REBUILT, not ALTERed. Two reasons, and the second is the binding one:
--
--  1. restatement_pairs is derived index state — a cache the shortlist refresh
--     rebuilds from git — so there is nothing here to preserve. Every existing
--     row was found by the title KNN and predates structural detection
--     entirely, so a full re-detection is what we want on the next session
--     regardless.
--  2. `ALTER TABLE ... ADD COLUMN` is NOT idempotent, and this schema's
--     recovery path re-runs a migration to find out whether an interrupted
--     body committed. That budget is already spent by 000019's ADD COLUMN; a
--     second one fails the re-run outright. DROP+CREATE re-runs cleanly, which
--     is the 000017 precedent (a rebuild, for its own reasons).
--
-- The cache STATE goes with the pairs, and leaving it would be the bug the
-- ReplaceRestatementPairs comment warns about: the state rows are what says
-- "already covered", so a cache that lost its pairs but kept its state would
-- never rebuild them.
--
-- No GraphSchemaVersion bump, for the reason 000018/000019 skipped it: there is
-- no derived state elsewhere that a rebuild would leave stale.
DROP TABLE IF EXISTS restatement_pairs;

CREATE TABLE restatement_pairs (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    a_path     TEXT NOT NULL,
    b_path     TEXT NOT NULL,
    a_fact_id  INTEGER NOT NULL,
    b_fact_id  INTEGER NOT NULL,
    title_cos  REAL NOT NULL,
    match_kind TEXT NOT NULL DEFAULT 'title-knn',
    PRIMARY KEY (branch_id, a_path, b_path)
);
CREATE INDEX restatement_pairs_rank
    ON restatement_pairs(branch_id, title_cos DESC);
CREATE INDEX restatement_pairs_a ON restatement_pairs(branch_id, a_fact_id);
CREATE INDEX restatement_pairs_b ON restatement_pairs(branch_id, b_fact_id);
CREATE INDEX restatement_pairs_match
    ON restatement_pairs(branch_id, match_kind, title_cos DESC);

DELETE FROM restatement_cache_state;
