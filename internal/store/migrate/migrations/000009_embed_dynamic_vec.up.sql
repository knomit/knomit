-- facts_vec is now created in Go (ensureFactsVec) at the active model's
-- dimension, because a vec0 table fixes its dimension at CREATE and cannot be
-- ALTERed. Drop the static FLOAT[768] virtual table (this also removes its
-- shadow tables); the startup rebuild recreates it. Derived state: rebuilt from
-- git, no data backfill.
DROP TABLE IF EXISTS facts_vec;
