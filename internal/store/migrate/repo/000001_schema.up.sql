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

-- Branch registry
CREATE TABLE IF NOT EXISTS branches (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    git_ref TEXT NOT NULL
);

-- Immutable fact content store (one row per unique path+content version)
CREATE TABLE IF NOT EXISTS facts (
    id              INTEGER PRIMARY KEY,
    path            TEXT NOT NULL,
    blob_hash       TEXT NOT NULL,
    title           TEXT NOT NULL,
    type            TEXT NOT NULL DEFAULT 'observation',
    domain          TEXT NOT NULL,
    entities        TEXT NOT NULL,
    confidence      REAL NOT NULL,
    sources         INTEGER NOT NULL,
    refs            TEXT NOT NULL,
    evidence_weight REAL NOT NULL DEFAULT 0,
    UNIQUE(path, blob_hash)
);

CREATE INDEX IF NOT EXISTS facts_type ON facts(type);

-- Branch view: which fact version each branch sees at each path
CREATE TABLE IF NOT EXISTS branch_facts (
    branch_id   INTEGER NOT NULL REFERENCES branches(id),
    path        TEXT NOT NULL,
    fact_id     INTEGER NOT NULL REFERENCES facts(id),
    commit_hash TEXT NOT NULL,
    UNIQUE(branch_id, path)
);
CREATE INDEX IF NOT EXISTS branch_facts_fact ON branch_facts(fact_id);

-- Trigger: clean up embeddings when a fact is deleted
CREATE TRIGGER IF NOT EXISTS facts_after_delete AFTER DELETE ON facts
BEGIN
    DELETE FROM facts_vec WHERE rowid = OLD.id;
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
-- Branch-agnostic: one row per (commit_hash, path). Branch visibility is
-- tracked separately in branch_commits below. Populated on open
-- (INSERT OR IGNORE) and appended after each write/sync.
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

-- Many-to-many: which branches see which commits.
-- A commit is on a branch iff it is reachable by walking parents from the branch ref.
-- Populated by populateCommitLog (git walk) and CreateBranch (clone from parent).
-- ON DELETE CASCADE: dropping a branch row also drops its visibility rows.
CREATE TABLE IF NOT EXISTS branch_commits (
    branch_id   INTEGER NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    commit_hash TEXT    NOT NULL,
    PRIMARY KEY (branch_id, commit_hash)
);
CREATE INDEX IF NOT EXISTS branch_commits_hash ON branch_commits (commit_hash);

-- Junction tables for indexed entity and domain filtering.
-- Populated in sync with facts.entities / facts.domain JSON columns.
-- ON DELETE CASCADE keeps them in sync when facts are deleted.
CREATE TABLE IF NOT EXISTS fact_entities (
    fact_id INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    entity  TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (fact_id, entity)
);
CREATE INDEX IF NOT EXISTS fact_entities_entity ON fact_entities(entity);

CREATE TABLE IF NOT EXISTS fact_domains (
    fact_id INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    domain  TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (fact_id, domain)
);
CREATE INDEX IF NOT EXISTS fact_domains_domain ON fact_domains(domain);
