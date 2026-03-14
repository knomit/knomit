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
