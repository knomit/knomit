-- First-mission rehearsal finding F5: a `message` column on the fire log.
--   message  a non-error result text: what a recipe's result row reports
--            when its outcome is a success (done | spawned | delivered), e.g.
--            "exit 0 cost=1.03". Before this column it was stored in `error`
--            for every outcome, so a successful run read as an error. `error`
--            now holds only failures (recipe-error, recipe-timeout,
--            unreachable, busy, script-error, ...). '' on every other row.
--
-- REBUILT, not ALTERed, for the reasons 000031 gives: `ALTER TABLE ... ADD
-- COLUMN` is not idempotent (upWithRecovery re-runs a body to learn whether an
-- interrupted one committed, and the rewound-head migration tests absorb only
-- one duplicate-column collision — kb conventions/store/migrations), and
-- `ALTER TABLE ... RENAME` re-parses a schema that names the sqlite-vec table.
-- The log is copied OUT (the 24 columns 000031 created), the table dropped and
-- recreated with the new column, and the log copied BACK with its ids, so
-- AUTOINCREMENT continues after the highest one. A re-run over an applied body
-- does the same; it would drop `message` values, but no row can be written
-- between the body's commit and the re-run (recovery runs at open), and the
-- table is LOCAL CACHE (never synced).
DROP TABLE IF EXISTS trigger_fires_000033;
CREATE TABLE trigger_fires_000033 AS SELECT id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at,
    recipe_source, recipe_rev, run_id
FROM trigger_fires;
DROP TABLE trigger_fires;
CREATE TABLE trigger_fires (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger          TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL,
    path             TEXT    NOT NULL DEFAULT '',
    episode          TEXT    NOT NULL DEFAULT '',
    source           TEXT    NOT NULL DEFAULT '',
    commit_hash      TEXT    NOT NULL DEFAULT '',
    trace            TEXT    NOT NULL DEFAULT '',
    outcome          TEXT    NOT NULL,
    error            TEXT    NOT NULL DEFAULT '',
    nonlinear        INTEGER NOT NULL DEFAULT 0,
    range_from       TEXT    NOT NULL,
    range_to         TEXT    NOT NULL,
    evaluated        INTEGER NOT NULL DEFAULT 0,
    paths            INTEGER NOT NULL DEFAULT 0,
    fires            INTEGER NOT NULL DEFAULT 0,
    fires_not_logged INTEGER NOT NULL DEFAULT 0,
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    diff_ms          INTEGER NOT NULL DEFAULT 0,
    change_ms        INTEGER NOT NULL DEFAULT 0,
    fired_at         INTEGER NOT NULL,
    recipe_source    TEXT    NOT NULL DEFAULT '',
    recipe_rev       TEXT    NOT NULL DEFAULT '',
    run_id           TEXT    NOT NULL DEFAULT '',
    message          TEXT    NOT NULL DEFAULT ''
);
INSERT INTO trigger_fires (id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at,
    recipe_source, recipe_rev, run_id)
SELECT id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at,
    recipe_source, recipe_rev, run_id
FROM trigger_fires_000033;
DROP TABLE trigger_fires_000033;
CREATE INDEX IF NOT EXISTS trigger_fires_branch ON trigger_fires(branch, id);
CREATE INDEX IF NOT EXISTS trigger_fires_run ON trigger_fires(branch, run_id) WHERE run_id != '';
