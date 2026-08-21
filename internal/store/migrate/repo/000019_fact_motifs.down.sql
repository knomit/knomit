DROP TABLE IF EXISTS fact_motifs;
-- facts.motifs is deliberately left in place. SQLite's DROP COLUMN is refused
-- while the column is named by an index or a live statement, and a stale
-- unread column costs one JSON literal per row. The 000012 origin migration
-- set the same precedent: junction and index come out, the parsed-cache column
-- stays.
