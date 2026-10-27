-- handle_mounts: the URL-scoped endpoint a binding handle was minted on.
--
-- A --repo or --lens bridge is fixed to one repo or lens by its URL, and
-- knomit_bind there mints handles that change only the BRANCH within it. Such
-- a handle must work on that endpoint and nowhere else, and a handle minted on
-- the unscoped /api/v1/mcp endpoint must not work on a scoped one. A row here
-- says which scoped mount minted the handle (mount_uid is the mount's pin,
-- repo:<uid> or lens:<uid>, the same key mount_experiments uses). NO ROW means
-- the handle was minted on the unscoped endpoint, which is every handle that
-- existed before this table.
--
-- Routing state, like binding_handles, so it is keyed on the HANDLE and never
-- on Mcp-Session-Id: two subagents sharing one connection each hold their own
-- handle, and that is what keeps their branches apart.
--
-- A SEPARATE TABLE, not a column on binding_handles, because this chain is
-- replayed as one concatenated script and admits idempotent CREATE ... IF NOT
-- EXISTS only (TestControl_UpMigrationsAreIdempotentDDL). The handle_experiments
-- precedent. The row is written in the same transaction as the handle, so a
-- scoped handle never exists without it, and Store.Purge collects orphans by
-- anti-join after deleting the handles.
--
-- NOTE, the same one 000005 carries: the idempotency guard splits this file on
-- the semicolon BEFORE it strips comments, so a semicolon anywhere in this
-- prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS handle_mounts (
    handle     TEXT PRIMARY KEY,
    mount_uid  TEXT NOT NULL,
    created_at INTEGER NOT NULL  -- Unix seconds
);
