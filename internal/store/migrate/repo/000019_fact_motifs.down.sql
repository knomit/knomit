DROP INDEX IF EXISTS fact_motifs_motif;
DROP TABLE IF EXISTS fact_motifs;
-- The column goes too, matching 000006 (kind) and 000012 (origin), which both
-- drop theirs. Keeping it would also break the down→up cycle: re-running the up
-- migration would hit "duplicate column name: motifs" and leave the schema
-- dirty.
ALTER TABLE facts DROP COLUMN motifs;
