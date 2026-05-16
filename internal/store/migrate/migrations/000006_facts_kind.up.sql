ALTER TABLE facts ADD COLUMN kind TEXT NOT NULL DEFAULT 'epistemic';
CREATE INDEX IF NOT EXISTS facts_kind ON facts(kind);
