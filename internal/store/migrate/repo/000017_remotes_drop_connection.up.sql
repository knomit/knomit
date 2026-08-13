-- Remote connection identity moved to <home>/control.db (repo_origins), so a
-- lost repository database can be re-cloned from a record that outlives it.
-- What stays here is STATUS: it describes this local replica's relationship to
-- the remote and is meaningless after a rehydrate.
--
-- This is the table-rebuild idiom rather than `ALTER TABLE remotes DROP COLUMN`
-- — NOT because of the SQLite version (the bundled library is 3.51.2, well past
-- the 3.35 floor), but because DROP COLUMN re-prepares every trigger in the
-- schema to check whether it references the doomed column. This schema always
-- has one that cannot be prepared: `facts_after_delete` (migration 000001)
-- deletes from `facts_vec`, and migration 000009 dropped `facts_vec` because a
-- vec0 table fixes its dimension at CREATE — Go recreates it at the active
-- model's dimension AFTER migrations run. So during any migration, DROP COLUMN
-- fails with "error in trigger facts_after_delete: no such table: facts_vec",
-- and it fails for EVERY database, not an unlucky few.
--
-- `ALTER TABLE ... RENAME TO` re-prepares triggers for the same reason, so the
-- rename below runs under legacy_alter_table, which skips the reference rewrite
-- (nothing references `remotes` — it has no indexes, triggers, views or foreign
-- keys, so there is nothing to rewrite). The pragma is restored afterwards.
--
-- Caveat on that restore: PRAGMAs are not transactional. If a statement below
-- fails, the surrounding transaction rolls the SCHEMA back but leaves
-- legacy_alter_table ON for the lifetime of that pooled connection. That is
-- tolerable only because a failed migration is already fatal — store.Open
-- returns the error and the process never reaches a state where it could reuse
-- the connection. Do not weaken that: a migration failure that got swallowed
-- would leak legacy ALTER TABLE semantics into normal operation.
PRAGMA legacy_alter_table=ON;

CREATE TABLE remotes_new (
    name             TEXT PRIMARY KEY,
    interval         INTEGER NOT NULL DEFAULT 300,
    last_sync_at     TEXT,
    last_status      TEXT,
    last_error       TEXT,
    push_interval    INTEGER NOT NULL DEFAULT 300,
    last_push_at     TEXT,
    last_push_status TEXT,
    last_push_error  TEXT
);

INSERT INTO remotes_new
    (name, interval, last_sync_at, last_status, last_error,
     push_interval, last_push_at, last_push_status, last_push_error)
SELECT name, interval, last_sync_at, last_status, last_error,
       push_interval, last_push_at, last_push_status, last_push_error
  FROM remotes;

DROP TABLE remotes;
ALTER TABLE remotes_new RENAME TO remotes;

PRAGMA legacy_alter_table=OFF;
