-- Git object store (used by internal/store/git/ storer)
CREATE TABLE IF NOT EXISTS objects (
    hash TEXT NOT NULL,
    type INTEGER NOT NULL,
    size INTEGER NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY (hash, type)
);

CREATE TABLE IF NOT EXISTS refs (
    name        TEXT PRIMARY KEY,
    target      TEXT NOT NULL,
    is_symbolic INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);

-- Metadata (unified schema versioning)
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Search index (no body column — body lives in objects via blob_hash)
CREATE TABLE IF NOT EXISTS facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    blob_hash   TEXT NOT NULL,
    type        TEXT NOT NULL DEFAULT 'observation',
    domain      TEXT NOT NULL,
    entities    TEXT NOT NULL,
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs             TEXT NOT NULL,
    commit_hash      TEXT NOT NULL,
    evidence_weight  REAL NOT NULL DEFAULT 0
);

-- Trigger: clean up embeddings when a fact is deleted
CREATE TRIGGER IF NOT EXISTS facts_after_delete AFTER DELETE ON facts
BEGIN
    DELETE FROM facts_vec WHERE rowid = OLD.rowid;
END;

-- Pipeline watermarks (track last-processed commit per tool per branch)
CREATE TABLE IF NOT EXISTS pipeline_watermarks (
    tool        TEXT NOT NULL,
    branch      TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    PRIMARY KEY (tool, branch)
);

-- Pipeline sessions
CREATE TABLE IF NOT EXISTS pipeline_sessions (
    id          TEXT PRIMARY KEY,
    tool        TEXT NOT NULL,
    branch      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- Pipeline work items
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

-- Remote sync configuration
CREATE TABLE IF NOT EXISTS remotes (
    name             TEXT PRIMARY KEY,
    url              TEXT NOT NULL,
    branch           TEXT NOT NULL DEFAULT 'main',
    interval         INTEGER NOT NULL DEFAULT 300,
    last_sync_at     TEXT,
    last_status      TEXT,
    last_error       TEXT,
    push_interval    INTEGER NOT NULL DEFAULT 300,
    last_push_at     TEXT,
    last_push_status TEXT,
    last_push_error  TEXT,
    auth_method      TEXT NOT NULL DEFAULT '',
    auth_token       TEXT NOT NULL DEFAULT ''
);

-- Tool sessions (paginated browsing/explain)
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

-- Denormalized commit log for O(1) activity queries and efficient path history.
-- Populated on open (INSERT OR IGNORE) and appended after each write/sync.
-- Insertion order mirrors commit age: oldest first → rowid order reflects recency.
CREATE TABLE IF NOT EXISTS commit_log (
    commit_hash  TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    committed_at INTEGER NOT NULL,  -- Unix seconds
    message      TEXT    NOT NULL,
    operation    TEXT    NOT NULL DEFAULT '',
    author_email TEXT    NOT NULL DEFAULT '',
    action       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (commit_hash, path)
);
CREATE INDEX IF NOT EXISTS commit_log_path_time ON commit_log (path, committed_at DESC);
CREATE INDEX IF NOT EXISTS commit_log_time      ON commit_log (committed_at DESC);
CREATE INDEX IF NOT EXISTS commit_log_operation ON commit_log (operation, committed_at DESC);
