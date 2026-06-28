-- Ephemeral session database schema.
--
-- This file is NOT a versioned migration. The session DB is a separate,
-- disposable SQLite file recreated empty on every Service.Open (see
-- initSessionSchema in service.go), so there is no prior data to migrate and no
-- version history to honor — the whole file is exec'd wholesale against the
-- fresh DB. Schema changes are plain edits here; they take effect on next start.
--
-- It holds only in-flight, process-runtime session/work-queue state that is NOT
-- derivable from git and NOT meaningfully durable across restarts. Durable
-- progress (pipeline_watermarks) deliberately stays in the main DB.

-- Tool sessions: paginated browsing/explain/explore/query cursors.
CREATE TABLE tool_sessions (
    id           TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    path_prefix  TEXT NOT NULL DEFAULT '',
    last_commit  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL              -- idle-reap heartbeat (bumped on every page)
);

CREATE TABLE tool_seen_paths (
    session_id TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    PRIMARY KEY (session_id, path)
);

CREATE TABLE tool_queue (
    session_id  TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    sort_key    INTEGER NOT NULL DEFAULT 0,   -- SQL-orderable consume order (was `depth`)
    state       TEXT NOT NULL DEFAULT '',     -- per-item JSON payload (query: frozen snippet)
    PRIMARY KEY (session_id, path, commit_hash)
);

CREATE INDEX tool_queue_session_sort ON tool_queue(session_id, sort_key);
CREATE INDEX tool_sessions_last_used ON tool_sessions(last_used_at);

-- Pipeline work-stealing sessions (review/distill/reflect/hypothesize).
CREATE TABLE pipeline_sessions (
    id           TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    phase        TEXT NOT NULL DEFAULT 'work',
    scoped       INTEGER NOT NULL DEFAULT 0, -- 1 when session was started with a scope filter
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL              -- idle-reap heartbeat (bumped on work-item access)
);

CREATE TABLE pipeline_work_items (
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

CREATE INDEX pipeline_sessions_last_used ON pipeline_sessions(last_used_at);
