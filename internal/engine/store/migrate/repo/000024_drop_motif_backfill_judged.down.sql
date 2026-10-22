-- Recreate motif_backfill_judged exactly as 000022 declared it.
--
-- Structure only: the ROWS are not recoverable, because the up-migration
-- dropped them and nothing else in the schema records which facts were judged
-- empty. A down-migrated store therefore has the table an old binary expects
-- and an empty backlog history, which reads as "nothing has been judged yet" —
-- correct as a shape, wrong as a history. Anything relying on this table after
-- a down-migration must treat it as reset, not restored.
CREATE TABLE IF NOT EXISTS motif_backfill_judged (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    fact_id    INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    judged_at  INTEGER NOT NULL,
    PRIMARY KEY (branch_id, fact_id)
);
