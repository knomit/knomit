CREATE TABLE IF NOT EXISTS verify_context (
    commit_hash TEXT    PRIMARY KEY,
    mode        TEXT    NOT NULL,
    signers     TEXT    NOT NULL,
    computed_at INTEGER NOT NULL
);
