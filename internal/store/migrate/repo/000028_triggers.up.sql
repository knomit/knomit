-- F07: the trigger dispatcher's two tables (see internal/store/triggers.go and
-- internal/repos/triggers.go). Both are LOCAL CACHE: never synced, rebuildable
-- (a lost watermark restarts the trigger at the head of the next advance; a
-- lost fire log loses operator history, never a fact).
--
-- trigger_watermarks: one row per (trigger name, branch) — the last head of the
-- agent branch this trigger fully processed. A SEPARATE table from
-- pipeline_watermarks on purpose: OpenExperiment copies every pipeline
-- watermark onto exp/<name>, and trigger bookmarks must never fork onto a
-- branch nothing fires on.
--
-- trigger_fires: the operator's fire log, capped at the newest 10,000 rows per
-- repo (the prune is `id <= MAX(id) - 10000`, a rowid-range delete). One row per
-- emitted fire or error outcome, plus exactly ONE run row per dispatcher run
-- (trigger = '', outcome = 'run') carrying the coalesced range and the run's
-- counts. `if-false` evaluations are counted in memory, never written.
-- fired_at is the only clock in F07 PR 1: written for the operator, never read
-- for ordering.
--
-- CREATE ... IF NOT EXISTS: the body must re-run cleanly (upWithRecovery).
CREATE TABLE IF NOT EXISTS trigger_watermarks (
    trigger     TEXT NOT NULL,
    branch      TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    PRIMARY KEY (trigger, branch)
);
CREATE TABLE IF NOT EXISTS trigger_fires (
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
CREATE INDEX IF NOT EXISTS trigger_fires_branch ON trigger_fires(branch, id);
