-- Rollback only. Recreates facts_vec at the legacy 768 dim; if the active model
-- uses a different dim, the next startup Rebuild (ensureFactsVec) heals it.
CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(embedding FLOAT[768] distance_metric=cosine);
