-- The record that makes backfill a ONE-TIME DRAIN rather than a standing job.
--
-- Backfill's backlog is facts that predate the motif field: the corpus did not
-- change, the SYSTEM did. That work is finite, and a session is non-empty only
-- while facts exist that have NEVER been judged.
--
-- Only a POSITIVE assignment used to leave a trace, so a fact an agent
-- correctly judged to carry no regularity was re-offered every session forever.
-- On a corpus with enough such facts they fill every slot and the sweep dies
-- before reaching the tail.
--
-- Keyed on fact_id, which is content-addressed for free: `facts` rows are
-- immutable and UNIQUE(path, blob_hash), so an EDITED fact is a different row
-- that has never been judged and is correctly back in the backlog. The
-- judgement was about the content, not the path.
--
-- Branch-scoped because a judgement is one agent's answer on its own branch;
-- another branch holding the same blob has not answered anything.
--
-- ON DELETE CASCADE: a fact whose row is gone cannot be re-offered, so its
-- judgement has nothing left to describe.
CREATE TABLE IF NOT EXISTS motif_backfill_judged (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    fact_id    INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    judged_at  INTEGER NOT NULL,
    PRIMARY KEY (branch_id, fact_id)
);
