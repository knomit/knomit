-- F03: a fact's optional `expires` (RFC 3339, frontmatter). Knomit never acts
-- on it — no hiding, no deletion — it is searchable and results are marked.
--
-- Two columns, both derived from the frontmatter at index time:
--   expires    the value AS WRITTEN (offset kept), NULL when absent, so a
--              result row can show exactly what the file says;
--   expires_at the same instant as unix seconds UTC, NULL when absent, which
--              is what every filter compares (RFC 3339 strings with different
--              offsets do not order correctly as text).
--
-- No data backfill here: derived index state is regenerated from git, never
-- data-migrated (architecture/store/f6db3a49). GraphSchemaVersion 5 → 6 forces
-- the per-branch Rebuild that fills both columns from every blob, which is
-- needed because a hand-written `expires:` line could already sit in a repo
-- and the pre-F03 parser dropped it.
ALTER TABLE facts ADD COLUMN expires TEXT;
ALTER TABLE facts ADD COLUMN expires_at INTEGER;
CREATE INDEX IF NOT EXISTS facts_expires_at ON facts(expires_at) WHERE expires_at IS NOT NULL;
