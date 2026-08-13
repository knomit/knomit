-- Recreate the session/work-queue tables in their pre-relocation shape (as of
-- migration 000001, plus the pipeline_sessions.phase column from 000005). This
-- restores schema only; the relocated runtime data is ephemeral and is not
-- recovered. pipeline_watermarks was never dropped, so it is not recreated here.
CREATE TABLE IF NOT EXISTS tool_sessions (
    id           TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    path_prefix  TEXT NOT NULL DEFAULT '',
    last_commit  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tool_seen_paths (
    session_id TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    PRIMARY KEY (session_id, path)
);
CREATE TABLE IF NOT EXISTS tool_queue (
    session_id  TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, path, commit_hash)
);
CREATE TABLE IF NOT EXISTS pipeline_sessions (
    id          TEXT PRIMARY KEY,
    tool        TEXT NOT NULL,
    branch      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    phase       TEXT NOT NULL DEFAULT 'work',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS pipeline_work_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL REFERENCES pipeline_sessions(id) ON DELETE CASCADE,
    step_type   TEXT NOT NULL,
    cluster_key TEXT NOT NULL,
    facts_json  TEXT NOT NULL,
    response    TEXT,
    priority    REAL NOT NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
