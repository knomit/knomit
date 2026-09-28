-- Reverse of 000031: rebuild trigger_fires without the three recipe columns,
-- keeping the log (the same copy-out / recreate / copy-back as the up).
DROP TABLE IF EXISTS trigger_fires_000031;
CREATE TABLE trigger_fires_000031 AS SELECT id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at
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
    fired_at         INTEGER NOT NULL
);
INSERT INTO trigger_fires (id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at)
SELECT id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at
FROM trigger_fires_000031;
DROP TABLE trigger_fires_000031;
CREATE INDEX IF NOT EXISTS trigger_fires_branch ON trigger_fires(branch, id);
