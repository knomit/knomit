-- Reverse of 000033: rebuild trigger_fires without `message`, keeping the log
-- (the same copy-out / recreate / copy-back as the up). The 000031 shape.
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
    run_id           TEXT    NOT NULL DEFAULT ''
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
