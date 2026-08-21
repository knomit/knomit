-- The motif field's storage (blueprint §1), mirroring fact_domains: a JSON
-- column on facts for the bulk rebuild to read, and a junction table for
-- indexed counting (TokenDF kind "motif").
--
-- BOTH are required, and the pairing is easy to get wrong. upsert() populates
-- the junction directly from the parsed record, but rebuildFacts repopulates
-- it with SQL over facts.<column> via json_each — so a junction WITHOUT a
-- column silently empties on the first full rebuild, while every unit test
-- that only drives the incremental path stays green while it happens. That is
-- what TestFactMotifs_SurvivesFullRebuild exists to catch.
--
-- Derived index state, rebuilt from git: no data backfill, because no fact in
-- any existing corpus carries a motif and EMPTY is therefore the correct state
-- for every row. For the same reason, deliberately NO GraphSchemaVersion bump
-- (the 000018 precedent, and architecture/store/f6db3a49 — "USUALLY a bump ...
-- but NOT ALWAYS"): the bump exists to force regeneration of state a rebuild
-- would otherwise leave stale, and there is no such state here.
--
-- Motifs are stored AS WRITTEN (roadmap MN3). Unlike fact_domains, which
-- stores knomit_canon_domain's output, NO canonicalization happens here: alias
-- resolution and canonical ids are Phase-2 derived state built FROM these rows
-- and never written back into them. COLLATE NOCASE is a matching convenience
-- shared with the sibling junctions; it changes no stored bytes.
ALTER TABLE facts ADD COLUMN motifs TEXT NOT NULL DEFAULT '[]';

CREATE TABLE IF NOT EXISTS fact_motifs (
    fact_id INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    motif   TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (fact_id, motif)
);
CREATE INDEX IF NOT EXISTS fact_motifs_motif ON fact_motifs(motif);
