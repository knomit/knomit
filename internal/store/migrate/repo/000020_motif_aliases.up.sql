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
    -- The merge rationale, denormalized from motif_judge_verdicts onto the
    -- alias rows of a judge-formed cluster, so `method='judge'` is legible
    -- where it is read. Refreshed on every rebuild; no independent lifetime.
    rationale    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (branch_id, motif)
);
CREATE INDEX IF NOT EXISTS motif_aliases_canonical
    ON motif_aliases(branch_id, canonical_id);
CREATE INDEX IF NOT EXISTS motif_aliases_cluster
    ON motif_aliases(branch_id, cluster_key);

-- What the judge pass has already decided about a pair of clusters.
--
-- A durable DECISION table, not a derivation — the same standing as Phase 0's
-- verdict record. Recomputing it would make every review session re-judge the
-- whole vocabulary, turning §3.1's "one bounded prompt" into a cost that grows
-- with the corpus. Persisting makes the pass INCREMENTAL: only vocabulary it
-- has not seen costs a slot.
--
-- Both answers are recorded, and that symmetry is the point. Storing only
-- merges leaves the pass half incremental: the YES answers get cheap while the
-- NO answers are re-litigated every session, so a stable corpus spends its
-- entire budget re-asking questions it already has answers to.
--
-- Still derived state under MN3 in the sense that matters: nothing here is
-- ever written back into a fact, and dropping the table costs re-judging.
CREATE TABLE IF NOT EXISTS motif_judge_verdicts (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    -- Canonical order: key_a < key_b, so the primary key collapses A-B and B-A,
    -- exactly as restatement_pairs does.
    --
    -- CLUSTER KEYS, not spellings: keys are stable under df shifts, so a
    -- decision recorded this session still identifies the same clusters next
    -- session. A verdict naming keys the corpus no longer has goes inert on its
    -- own — the rebuild overlay only applies what is present, so a retired
    -- vocabulary never resurrects itself and nothing needs cleaning up.
    key_a      TEXT NOT NULL,
    key_b      TEXT NOT NULL,
    judged_at  TEXT NOT NULL,
    -- 1 = merged, 0 = declined.
    merged     INTEGER NOT NULL DEFAULT 1,
    -- The judge's own words for the shared mechanism (designer ruling
    -- 2026-08-21: a merge counts only when the judge names it). Empty on a
    -- decline, REQUIRED on a merge and refused at the write path — which is
    -- what makes "name it or it does not count" enforceable rather than
    -- aspirational, and leaves an audit trail a later harden pass can read.
    rationale  TEXT NOT NULL DEFAULT '',
    -- The cluster MEMBERSHIP each side had when the verdict was given, sorted
    -- and joined. A verdict binds only while both sides still mean what they
    -- meant: new carrier spellings can genuinely change the answer, so a
    -- membership change re-eligibilizes the pair. Structural expiry, with no
    -- staleness check and no cleanup job — the same principle as Phase 0
    -- keying its verdicts on content-addressed fact ids.
    members_a  TEXT NOT NULL DEFAULT '',
    members_b  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (branch_id, key_a, key_b)
);
CREATE INDEX IF NOT EXISTS motif_judge_verdicts_branch
    ON motif_judge_verdicts(branch_id);
