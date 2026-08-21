-- Judge merges: the LLM clustering pass's decisions (blueprint §3.1 step 1,
-- second half), kept SEPARATE from the mechanical layer in motif_aliases.
--
-- Why a durable table rather than recomputing: a judge merge is a DECISION,
-- not a derivation — the same shape as Phase 0's restatement_verdicts. If
-- merges were rebuilt from scratch each session, every review would have to
-- re-judge the entire vocabulary, and §3.1's "one bounded prompt" would become
-- an unbounded per-session cost that grows with the corpus. Persisting them
-- makes the judge pass INCREMENTAL: only vocabulary it has not seen needs a
-- slot.
--
-- Still derived state under MN3, in the sense MN3 means: nothing here is ever
-- written back into a fact, and dropping the table costs only re-judging.
--
-- A merge names two CLUSTER KEYS, not two spellings. Cluster keys are stable
-- under df shifts (unlike the representative spelling), so a decision recorded
-- this session still identifies the same clusters next session.
--
-- A merge whose keys no longer exist in the corpus goes inert on its own: the
-- overlay in RebuildAliases only applies merges whose keys are present, so a
-- retired vocabulary does not resurrect itself. Nothing needs to clean up.
CREATE TABLE IF NOT EXISTS motif_judge_merges (
    branch_id  INTEGER NOT NULL REFERENCES branches(id),
    -- Canonical order: key_a < key_b, so the primary key collapses A-B and
    -- B-A, exactly as restatement_pairs does.
    key_a      TEXT NOT NULL,
    key_b      TEXT NOT NULL,
    judged_at  TEXT NOT NULL,
    PRIMARY KEY (branch_id, key_a, key_b)
);
CREATE INDEX IF NOT EXISTS motif_judge_merges_branch
    ON motif_judge_merges(branch_id);
