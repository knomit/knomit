-- Provenance stamp: how a fact came to exist. Orthogonal to kind/type.
-- Default 'authored' covers legacy rows; synthesis rows are backfilled to
-- 'distilled' here for immediate correctness, but the durable default lives
-- in fact.ParseFact (git is the source of truth; this table is a cache).
ALTER TABLE facts ADD COLUMN origin TEXT NOT NULL DEFAULT 'authored';
CREATE INDEX IF NOT EXISTS facts_origin ON facts(origin);

-- Best-effort immediate backfill so queries are correct before the next
-- full index rebuild re-parses files (ParseFact applies the same rule).
UPDATE facts SET origin = 'distilled' WHERE type = 'synthesis';
