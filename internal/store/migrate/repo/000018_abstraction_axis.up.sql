-- The abstraction axis (title-only embeddings) and the restatement shortlist
-- built on it. Both exist to close gotchas/synthesize/prune-scope/c40d6748:
-- prune's judge only ever sees facts that landed in the same cluster, so a
-- restatement whose halves cluster apart is judged by nothing.
--
-- Derived index state: rebuilt from git, no data backfill here. Deliberately NO
-- GraphSchemaVersion bump — the bump exists to force regeneration of state a
-- rebuild would otherwise leave stale, and EMPTY is the correct state for
-- everything below: the review pipeline fills the axis lazily under its own
-- latency budget, and the pair cache is derived from whatever the axis holds
-- (architecture/store/f6db3a49 — "USUALLY a bump ... but NOT ALWAYS").
--
-- fact_titles_vec (the axis itself) is NOT created here. A vec0 table fixes its
-- dimension at CREATE and cannot be ALTERed, so like facts_vec it is
-- code-managed at the active model's dimension (ensureFactTitlesVec in
-- vec_table.go; the 000009 precedent). This file ships the plain tables and the
-- delete trigger only.
--
-- The axis is keyed by facts.id, which is content-addressed
-- (UNIQUE(path, blob_hash)), so an edited fact is a NEW row with no vector.
-- "Rows lacking a vector" is therefore exactly the delta, and the cache is
-- watermark-incremental with no bookkeeping of its own.
CREATE TRIGGER IF NOT EXISTS facts_after_delete_title_vecs AFTER DELETE ON facts
BEGIN
    DELETE FROM fact_titles_vec WHERE rowid = OLD.id;
END;

-- Standing candidate pairs, per branch. Canonical order: a_path < b_path, with
-- the ids ordered to match, so the primary key collapses A-B and B-A.
CREATE TABLE IF NOT EXISTS restatement_pairs (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    a_path     TEXT NOT NULL,
    b_path     TEXT NOT NULL,
    a_fact_id  INTEGER NOT NULL,
    b_fact_id  INTEGER NOT NULL,
    title_cos  REAL NOT NULL,
    PRIMARY KEY (branch_id, a_path, b_path)
);
CREATE INDEX IF NOT EXISTS restatement_pairs_rank
    ON restatement_pairs(branch_id, title_cos DESC);
CREATE INDEX IF NOT EXISTS restatement_pairs_a ON restatement_pairs(branch_id, a_fact_id);
CREATE INDEX IF NOT EXISTS restatement_pairs_b ON restatement_pairs(branch_id, b_fact_id);

-- Which fact ids the pair cache was last built over, per branch. The symmetric
-- difference against the live set IS the delta: no watermark column, and
-- nothing to migrate when the corpus changes shape.
CREATE TABLE IF NOT EXISTS restatement_cache_state (
    branch_id INTEGER NOT NULL REFERENCES branches(id),
    fact_id   INTEGER NOT NULL,
    PRIMARY KEY (branch_id, fact_id)
);

-- Judge outcomes for shortlist-originated prune items — the ONLY enforcement
-- input in this design. Two consumers: the trailing resolution-rate that funds
-- or defunds a corpus's shortlist, and the kept-pair guard that stops a
-- declined pair from being re-minted by a later neighbour rescan.
--
-- `resolved`, not `merged`: a judge that consolidates a restatement by
-- RETRACTING the redundant half has done exactly the work this mechanism
-- exists to buy, and counting only merges would defund a corpus that is
-- consolidating successfully by another route.
--
-- Fact ids, not just paths, because ids are content-addressed: "the judge kept
-- this pair" expires structurally the moment either fact is edited, with no
-- hash comparison and no staleness. Paths ride along because every human and
-- log line that reads these rows thinks in paths.
--
-- Losing these rows is safe. An empty window reads as "optimistic", which is
-- the cold-start posture anyway.
CREATE TABLE IF NOT EXISTS restatement_verdicts (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    branch_id INTEGER NOT NULL REFERENCES branches(id),
    a_path    TEXT NOT NULL,
    b_path    TEXT NOT NULL,
    a_fact_id INTEGER NOT NULL,
    b_fact_id INTEGER NOT NULL,
    resolved  INTEGER NOT NULL,
    judged_at TEXT NOT NULL
);

-- How many sessions a defunded corpus has waited since its last probe.
--
-- Without this the throttle is a LATCH, not a governor: a defunded corpus
-- emits nothing, so it produces no verdicts, so nothing can ever restore it.
-- The counter buys back the missing feedback at a bounded price — one probe
-- pair every Nth session — so recovery stays data-driven rather than
-- configured.
CREATE TABLE IF NOT EXISTS restatement_throttle_state (
    branch_id            INTEGER PRIMARY KEY REFERENCES branches(id),
    sessions_since_probe INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS restatement_verdicts_recent
    ON restatement_verdicts(branch_id, id DESC);
CREATE INDEX IF NOT EXISTS restatement_verdicts_pair
    ON restatement_verdicts(branch_id, a_fact_id, b_fact_id);
