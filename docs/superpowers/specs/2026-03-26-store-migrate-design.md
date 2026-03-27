# Design: Consolidated Schema Migrations via `store/migrate`

**Date:** 2026-03-26
**Status:** Approved

## Problem

Schema is currently spread across three locations with no version tracking:

1. `internal/store/schema.sql` — full table schema, embedded in `service.go`
2. `internal/git/store.go` — exports `Schema` constant, a duplicate subset (git tables + `commit_log`) used by tests
3. `internal/store/service.go` — 9 ad-hoc `ALTER TABLE`/`RENAME TABLE` SQL strings in a slice, run on every startup with errors silently ignored (no tracking of what has or hasn't been applied)

Additionally, vec0 virtual table creation uses `fmt.Sprintf` with a configurable dimension (always 768 in practice), and GraphQLite is initialized via a bare `SELECT cypher('RETURN 1')` call — neither is tracked as a migration.

## Goals

- Single source of truth for all schema: standard tables, vec0, and GraphQLite initialization
- Proper migration version tracking via `golang-migrate`
- Remove the `git.Schema` duplicate constant
- Remove the ad-hoc migration slice from `service.go`
- `storegit.NewMemoryStorer` requires no external schema string

## Approach: Single consolidated initial migration + extension migrations

All schema lives under `internal/store/migrate/migrations/` as numbered `.up.sql` files. No down migrations.

## Directory Structure

```
internal/store/migrate/
  migrations/
    000001_schema.up.sql       -- all standard SQLite tables (current schema.sql verbatim)
    000002_facts_vec.up.sql    -- CREATE VIRTUAL TABLE facts_vec USING vec0(FLOAT[768])
    000003_graphqlite.up.sql   -- SELECT cypher('RETURN 1')  [no-transaction]
  migrate.go                   -- Core(db) and All(db) exported functions + embedded FS
```

## `migrate.go` API

```go
package migrate

// Core applies only standard SQLite migrations (000001).
// Works with the plain "sqlite3" driver — no extensions required.
// Used by storegit.NewMemoryStorer and internal/git tests.
func Core(db *sql.DB) error

// All applies all migrations including vec0 and GraphQLite (000001–000003).
// db must be opened with the "sqlite3_knomit" driver (extensions loaded).
// Called by store.Open.
func All(db *sql.DB) error
```

Both functions use `golang-migrate` with an `iofs` embedded-FS source and the `sqlite3` database driver. `migrate.ErrNoChange` is treated as success (not an error).

## Migration Files

### `000001_schema.up.sql`
Verbatim copy of the current `schema.sql`. No changes needed — the file already reflects the final state, absorbing all past ad-hoc changes. All DDL uses `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`, making it idempotent against existing databases.

### `000002_facts_vec.up.sql`
```sql
CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(embedding FLOAT[768] distance_metric=cosine);
```
768 is hardcoded. The `WithVecDimension` option is removed. `CREATE VIRTUAL TABLE IF NOT EXISTS` is idempotent.

### `000003_graphqlite.up.sql`
```sql
-- migrate:no-transaction
SELECT cypher('RETURN 1');
```
The `-- migrate:no-transaction` directive disables `BEGIN/COMMIT` wrapping for this file. `SELECT cypher('RETURN 1')` bootstraps GraphQLite's EAV tables; it is idempotent.

## Migration ordering rationale

- 001 first: standard tables must exist before any extension-dependent objects reference them
- 002 second: vec0 virtual table (extension-dependent, but no dependency on GraphQLite)
- 003 last: GraphQLite initialization (only requires the extension to be loaded, not vec0)

## What Changes

| Location | Change |
|---|---|
| `internal/store/migrate/` | New package — migration files + `migrate.go` |
| `internal/store/service.go` | Remove `schema.sql` embed, ad-hoc migrations slice, vec0 `fmt.Sprintf`, and `SELECT cypher(...)`. Call `migrate.All(db)` instead. |
| `internal/store/schema.sql` | Deleted — content moves to `000001_schema.up.sql` |
| `internal/store/index.go` | Remove `WithVecDimension` option and `vecDim` field; remove duplicate vec0 DDL |
| `internal/git/store.go` | Delete `Schema` exported constant |
| `internal/store/git/storer.go` | `NewMemoryStorer()` drops `schema string` param; calls `migrate.Core(db)` internally |
| Test helpers | `newTestStorer`, `newInternalTestStorer`, `newWebTestStore` — remove `git.Schema` argument |

## Existing Database Compatibility

All migrations are idempotent (`IF NOT EXISTS`, or safe re-execution for `SELECT cypher(...)`). When an existing database is opened for the first time after this change:

1. `golang-migrate` creates the `schema_migrations` table
2. Runs migration 001 — all `CREATE TABLE IF NOT EXISTS` statements succeed silently
3. Runs migration 002 — `CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec` succeeds silently
4. Runs migration 003 — `SELECT cypher('RETURN 1')` runs again (harmless)
5. All three versions are recorded; subsequent opens skip them

No manual baseline or forced-version step is needed.

## Dependencies to Add

```
github.com/golang-migrate/migrate/v4
```

Specifically uses:
- `github.com/golang-migrate/migrate/v4/source/iofs` — embedded FS source
- `github.com/golang-migrate/migrate/v4/database/sqlite3` — SQLite driver
