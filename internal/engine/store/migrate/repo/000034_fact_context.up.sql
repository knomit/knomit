-- F22: a fact's optional `context:` map (frontmatter), short typed labels
-- declared per topic in the ontology. Indexed so knomit_query can filter on it.
--
-- A SIDE TABLE, not facts columns, for the reason fact_expires (000026) is
-- one: `ALTER TABLE ... ADD COLUMN` is not idempotent and the migration
-- recovery path's one-collision budget is spent; CREATE ... IF NOT EXISTS
-- re-runs cleanly.
--
-- One row per (fact VERSION, key), keyed by the immutable, content-addressed
-- facts row, so a new version of a fact gets its own rows. Rows of superseded
-- or retracted versions stay (the same as fact_expires): liveness is
-- branch_facts, and every reader joins the branch's live facts row and never
-- reads this table alone.
--   value  the canonical text: the string itself, true/false, or a number in
--          its shortest form. Exact-match filters compare against it.
--   num    the number for a numeric value (NULL otherwise), ready for range
--          filters later.
--
-- No data backfill here: derived index state is regenerated from git.
-- GraphSchemaVersion 6 -> 7 forces the per-branch Rebuild that fills the table
-- from every blob, needed because a hand-written `context:` could already sit
-- in a repo and a pre-F22 parser dropped it.
CREATE TABLE IF NOT EXISTS fact_context (
    fact_id INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    key     TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    num     REAL,
    PRIMARY KEY (fact_id, key)
);
CREATE INDEX IF NOT EXISTS fact_context_key_value ON fact_context(key, value);
