-- authz (unified authn/z plan, P1): the role seed, principal -> role
-- bindings and the bindings version, for internal/foundation/authz.
--
-- auth_roles and auth_role_permissions hold the fixed role -> permission
-- table. They are NOT seeded here: this chain admits idempotent CREATE ... IF
-- NOT EXISTS only (TestControl_UpMigrationsAreIdempotentDDL), so the rows are
-- written at every boot by authz.Store.SeedRoles from the Go table authz.Seed,
-- which is the single authority.
--
-- auth_role_bindings binds a principal (authz.Principal.String, e.g.
-- os:uid:1000, token:<id>, anonymous) to a role, instance-wide. The owner is
-- implicit and never has a row. created_at is Unix seconds.
--
-- auth_state is a small key/value table: bindings_version (bumped by every
-- change to bindings or to the role seed, the materializer's cache key) and
-- marker:<name> rows for one-shot boot steps.
--
-- NOTE: the idempotency guard splits this file on the semicolon BEFORE it
-- strips comments, so a semicolon anywhere in this prose reads as a statement
-- break. Do not put one here.
CREATE TABLE IF NOT EXISTS auth_roles (
    name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS auth_role_permissions (
    role       TEXT NOT NULL,
    permission TEXT NOT NULL,
    PRIMARY KEY (role, permission)
);

CREATE TABLE IF NOT EXISTS auth_role_bindings (
    principal  TEXT NOT NULL,
    role       TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (principal, role)
);

CREATE TABLE IF NOT EXISTS auth_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
