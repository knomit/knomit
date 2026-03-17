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
    refs        TEXT NOT NULL,
    commit_hash TEXT NOT NULL
);

-- Synthesis tracking
CREATE TABLE IF NOT EXISTS synthesis_log (
    recipe          TEXT PRIMARY KEY,
    last_commit     TEXT NOT NULL,
    run_at          TEXT NOT NULL,
    facts_processed INTEGER NOT NULL DEFAULT 0
);

-- Trigger: clean up embeddings when a fact is deleted
CREATE TRIGGER IF NOT EXISTS facts_after_delete AFTER DELETE ON facts
BEGIN
    DELETE FROM facts_vec WHERE rowid = OLD.rowid;
END;

-- Review watermarks (track last-reviewed commit per branch)
CREATE TABLE IF NOT EXISTS review_watermarks (
    branch      TEXT PRIMARY KEY,
    commit_hash TEXT NOT NULL
);

-- Review sessions
CREATE TABLE IF NOT EXISTS review_sessions (
    id          TEXT PRIMARY KEY,
    branch      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- Review work items
CREATE TABLE IF NOT EXISTS review_work_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL REFERENCES review_sessions(id) ON DELETE CASCADE,
    step_type   TEXT NOT NULL,
    cluster_key TEXT NOT NULL,
    facts_json  TEXT NOT NULL,
    response    TEXT,
    priority    REAL NOT NULL,
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
