-- F07 PR 2: the trigger dispatcher's third table, the `on: due` marks (see
-- internal/store/triggers.go DueMarks/AdvanceTriggerWatermarks and the sweep in
-- internal/repos/triggers.go). LOCAL CACHE like the other two: never synced,
-- never copied onto exp/<name>, untouched by an index rebuild (rebuildFacts
-- writes facts, fact_expires, the junctions and branch_facts only), so a
-- rebuild neither re-arms a processed fact nor forgets a mark; a lost table
-- (a store swap) makes the still-overdue facts fire once more (at-least-once).
--
-- One row per (branch, trigger, path): "this trigger has PROCESSED this path at
-- this due instant" — written for EVERY evaluated outcome (emitted, if-false,
-- if-error, if-timeout, unparseable), exactly as a watermark advances whatever
-- `if` said; otherwise an `if`-false due fact would be re-evaluated on every
-- run for as long as it is live. The column keeps the design's name fired_at.
--
-- Keyed per TRIGGER and per due INSTANT, not per fact version (maintainer
-- ruling 2026-09-28): two `due` triggers on one fact keep separate marks; a
-- body-only edit keeps expires_at and does not re-fire; changing the fact's
-- `expires` gives a different expires_at, so the fact fires again when that
-- instant passes and the row is replaced. The key starts with branch so the
-- per-run read `WHERE branch = ?` is an index range. Marks of a trigger NAME
-- that leaves the ontology are deleted with its bookmark (one rule for both
-- bookkeeping tables); marks for paths no longer live stay (tiny, bounded by
-- the distinct due paths per trigger): same path + same instant = processed.
--
-- The candidates the marks complement are the liveness join
-- fact_expires ⋈ branch_facts(agent branch), confirmed at the head's tree: a
-- retracted, deleted, merged-away, rewound or replaced fact is never a
-- candidate. fact_expires itself is F03's and is not changed here.
--
-- Times are UTC: expires_at and fired_at are Unix seconds (the same instant
-- everywhere); the dispatcher reads its clock once per run as UTC.
--
-- CREATE ... IF NOT EXISTS: the body must re-run cleanly (upWithRecovery).
CREATE TABLE IF NOT EXISTS trigger_due_fires (
    branch     TEXT    NOT NULL,
    trigger    TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    fired_at   INTEGER NOT NULL,
    PRIMARY KEY (branch, trigger, path)
);
