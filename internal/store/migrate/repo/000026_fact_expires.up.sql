-- F03: a fact's optional `expires` (RFC 3339, frontmatter). Knomit never acts
-- on it — no hiding, no deletion — it is searchable and results are marked.
--
-- A SIDE TABLE, not a facts column, and that is binding: `ALTER TABLE ... ADD
-- COLUMN` is not idempotent, and the migration recovery path (migrate.go,
-- upWithRecovery) re-runs a migration to learn whether an interrupted body
-- committed, tolerating exactly ONE "duplicate column" collision. 000019's
-- ADD COLUMN already spent that budget (000023's header says the same and
-- rebuilt its table for this reason); `facts` cannot be dropped and recreated.
-- CREATE ... IF NOT EXISTS re-runs cleanly.
--
-- One row per DATED fact version, keyed by the immutable, content-addressed
-- facts row (so a new version of a fact gets its own row):
--   expires    the value AS WRITTEN (offset kept), for display;
--   expires_at the same instant as unix seconds UTC, which is what every
--              filter compares (RFC 3339 strings with different offsets do not
--              order correctly as text).
-- Reads LEFT JOIN it; an undated fact simply has no row.
--
-- No data backfill here: derived index state is regenerated from git, never
-- data-migrated (architecture/store/f6db3a49). GraphSchemaVersion 5 -> 6 forces
-- the per-branch Rebuild that fills the table from every blob, which is needed
-- because a hand-written `expires:` line could already sit in a repo and the
-- pre-F03 parser dropped it.
CREATE TABLE IF NOT EXISTS fact_expires (
    fact_id    INTEGER PRIMARY KEY REFERENCES facts(id) ON DELETE CASCADE,
    expires    TEXT    NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS fact_expires_at ON fact_expires(expires_at);
