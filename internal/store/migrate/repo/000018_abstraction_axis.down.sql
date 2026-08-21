DROP TRIGGER IF EXISTS facts_after_delete_title_vecs;
DROP TABLE IF EXISTS restatement_verdicts;
DROP TABLE IF EXISTS restatement_cache_state;
DROP TABLE IF EXISTS restatement_pairs;
-- fact_titles_vec is code-managed (ensureFactTitlesVec), so it is dropped here
-- rather than by a CREATE this file never issued.
DROP TABLE IF EXISTS fact_titles_vec;
