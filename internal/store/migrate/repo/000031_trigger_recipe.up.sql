-- F07 PR 5: three fire-log columns for `do: run` and knomit.run (see
-- internal/store/triggers.go and internal/repos/trigger_recipe.go):
--   recipe_source  repo | local — where the recipe that ran came from: the
--                  tip of the repo's main branch, or <home>/recipes on this
--                  machine. NOT `source`, which is the episode's source
--                  (local | merged | due).
--   recipe_rev     the recipe's blob hash at main's tip (repo), or the first
--                  12 hex of the sha256 of its content (local): which exact
--                  code ran.
--   run_id         the run id knomit minted when it started the recipe
--                  (run-<32 hex>): the `started` row and the later result row
--                  carry the same one, so GET …/triggers?run=<id> finds both.
-- All three are '' on every other row, like the table's other text columns.
--
-- REBUILT, not ALTERed: `ALTER TABLE ... ADD COLUMN` is not idempotent and the
-- recovery path (migrate.go, upWithRecovery) re-runs a migration body to learn
-- whether an interrupted one committed (000023 and 000026 say the same). Nor
-- `ALTER TABLE ... RENAME`: SQLite re-parses the whole schema for it, and the
-- facts triggers name the sqlite-vec virtual table, which is not loaded here.
-- So the log is copied OUT, the table dropped and recreated, and the log
-- copied BACK. A re-run over an already-applied body does the same and loses
-- nothing (no row can be written between the body's commit and the re-run:
-- recovery runs at open). The table is LOCAL CACHE (never synced); the copy
-- keeps ids, so AUTOINCREMENT continues after the highest one.
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
    fired_at         INTEGER NOT NULL,
    recipe_source    TEXT    NOT NULL DEFAULT '',
    recipe_rev       TEXT    NOT NULL DEFAULT '',
    run_id           TEXT    NOT NULL DEFAULT ''
);
INSERT INTO trigger_fires (id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at)
SELECT id, trigger, branch, path, episode, source, commit_hash, trace, outcome, error,
    nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at
FROM trigger_fires_000031;
DROP TABLE trigger_fires_000031;
CREATE INDEX IF NOT EXISTS trigger_fires_branch ON trigger_fires(branch, id);
-- The lookup by run id (?run=<id>): at most two rows per id.
CREATE INDEX IF NOT EXISTS trigger_fires_run ON trigger_fires(branch, run_id) WHERE run_id != '';
