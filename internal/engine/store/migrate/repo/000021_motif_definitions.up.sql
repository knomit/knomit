-- One glossary definition per motif CLUSTER (blueprint §3.2).
--
-- Keyed on cluster_key, never on canonical_id (designer rider 2026-08-21).
-- canonical_id is the highest-df member spelling and FLIPS as usage shifts; the
-- cluster it names has not changed, and a definition keyed to the
-- representative would be orphaned by a change that meant nothing.
--
-- Definitions are authored BLIND — from the name alone, with no carrier facts
-- in the prompt. That is what makes the generic register achievable rather than
-- merely requested: a writer who never saw the carriers cannot name the systems
-- they are about.
--
-- Derived state (MN3): nothing here is written back into a fact, and dropping
-- the table costs one authoring pass.
CREATE TABLE IF NOT EXISTS motif_definitions (
    branch_id   INTEGER NOT NULL REFERENCES branches(id),
    cluster_key TEXT NOT NULL,
    definition  TEXT NOT NULL,
    -- The cluster MEMBERSHIP this definition was authored over, sorted and
    -- joined. Staleness is a COMPARISON, not a flag anyone sets: a definition
    -- needs refreshing exactly when this no longer matches the cluster's
    -- current membership.
    --
    -- Level-triggered, so there is no lifecycle to get wrong — no path that
    -- changes membership has to remember to mark anything, and none can forget.
    -- It also catches every cause of drift rather than one: a judge merge, a
    -- new spelling joining mechanically, or a member retiring when its last
    -- carrier is rewritten all move membership, and all three should prompt a
    -- fresh definition. A flag set by the merge path would have caught only the
    -- first (kb/decisions/ai/agents/reliability/reconciliation/b844c2b5).
    --
    -- A stale definition is kept and used as INTERIM rather than dropped
    -- (designer ruling): a judge merge asserts the phrasings name the SAME
    -- mechanism, so the survivor's definition is approximately right for the
    -- union. Gapping the cluster would be worse than a slightly wide sentence.
    members     TEXT NOT NULL,
    authored_at TEXT NOT NULL,
    PRIMARY KEY (branch_id, cluster_key)
);
