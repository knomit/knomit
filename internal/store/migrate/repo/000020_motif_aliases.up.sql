-- Alias resolution: the mapping from a motif spelling AS WRITTEN to the
-- canonical id its cluster is known by (blueprint §3.1).
--
-- DERIVED STATE, and nothing here is ever written back into a fact (MN3). The
-- authored strings in facts.motifs and fact_motifs remain the claim; this table
-- only records what the corpus's own vocabulary has been resolved to. Dropping
-- every row and rebuilding reproduces the mechanical layer exactly, which is
-- the property the MN3 conformance test asserts.
--
-- Per-branch, like every other derived index here: two branches can hold
-- different vocabularies and neither's resolution is valid for the other.
--
-- No GraphSchemaVersion bump, for the 000018 reason: EMPTY is the correct
-- state after a rebuild. The mechanical layer refills on the next review
-- session under its own budget, and an empty table degrades to "every motif is
-- its own singleton cluster" — which is exactly the pre-alias behaviour, not a
-- broken one.
CREATE TABLE IF NOT EXISTS motif_aliases (
    branch_id    INTEGER NOT NULL REFERENCES branches(id),
    -- The spelling as authored. COLLATE NOCASE to match fact_motifs, so a
    -- hand-edited file that capitalised a motif still resolves.
    motif        TEXT NOT NULL COLLATE NOCASE,
    -- The cluster's representative spelling. A member of the cluster, not a
    -- synthetic key: this id is shown in the §6 explain surface and in the
    -- backfill prompt, where a normalized token string would read as a bug.
    canonical_id TEXT NOT NULL COLLATE NOCASE,
    -- The cluster's STABLE identity: min() of its members' mechanical grouping
    -- keys (sorted stemmed token multiset). Distinct from canonical_id, and the
    -- distinction is load-bearing — canonical_id is the highest-df member
    -- spelling, so it FLIPS as usage shifts. Anything keyed to a cluster across
    -- sessions (definitions, above all) must key on this; a definition keyed to
    -- the representative would be orphaned by a flip that changed nothing about
    -- what the cluster means. Display canonical_id; key on cluster_key.
    --
    -- min() rather than the representative's own key so a judge merge of two
    -- mechanical clusters yields one deterministic survivor, and so the key is
    -- independent of df entirely.
    cluster_key  TEXT NOT NULL,
    -- 'canonical' = the mechanical stem/canonicalize layer.
    -- 'judge'     = the LLM clustering pass merged this spelling in.
    -- Kept apart so the LLM layer can be discarded and rebuilt without
    -- disturbing the mechanical one, and so a judge merge is auditable.
    method       TEXT NOT NULL,
    PRIMARY KEY (branch_id, motif)
);
CREATE INDEX IF NOT EXISTS motif_aliases_canonical
    ON motif_aliases(branch_id, canonical_id);
CREATE INDEX IF NOT EXISTS motif_aliases_cluster
    ON motif_aliases(branch_id, cluster_key);
