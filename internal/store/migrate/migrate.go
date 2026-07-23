package migrate

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/rs/zerolog/log"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Core applies only the standard SQLite migrations (version 1: all base tables).
// Works with the plain "sqlite3" driver — no extensions required.
// Used by storegit.NewMemoryStorer and internal/git tests.
//
// Core deliberately does NOT self-heal a dirty version the way All does. It
// targets version 1 explicitly, so rewinding a database that is dirty at some
// LATER version would make the retry migrate DOWN to 1 and destroy schema. Its
// callers only ever hand it a fresh :memory: database, where the dirty state
// cannot arise.
func Core(db *sql.DB) error {
	m, err := newMigrator(db)
	if err != nil {
		return err
	}
	if err := m.Migrate(1); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate.Core: %w", err)
	}
	return nil
}

// All applies every migration, including the vec0 table and the property-graph
// schema. db must be opened with the "sqlite3_knomit" driver (sqlite-vec loaded).
// Called by store.Open.
func All(db *sql.DB) error {
	m, err := newMigrator(db)
	if err != nil {
		return err
	}
	if err := upWithRecovery(m); err != nil {
		return fmt.Errorf("migrate.All: %w", err)
	}
	return nil
}

// upWithRecovery runs every pending migration, recovering ONCE from a dirty
// version left behind by an interrupted migration.
//
// golang-migrate flips schema_migrations.dirty before running a migration body
// and clears it after, in transactions SEPARATE from the body itself. A crash,
// SIGKILL, or power loss anywhere in that span leaves dirty = 1 permanently,
// and every subsequent open fails with ErrDirty — which for knomit means the
// repo is dropped at startup and looks, from the API, like data loss (#33).
//
// Recovery is safe because the migration BODY is transactional: an interrupted
// migration leaves the schema either fully applied or fully unapplied, never
// torn. Rewinding the bookkeeping one step and re-running is therefore either a
// genuine retry or a no-op over idempotent DDL.
//
// The dirty flag does not say WHICH side of that boundary the crash fell on —
// the driver runs the body and the dirty-clearing UPDATE in separate
// transactions — so recovery tries the two possibilities in turn, each EXACTLY
// once:
//
//  1. Body never landed. Rewind to N-1 and re-run N. This is the case that
//     must work, so it is tried first.
//  2. Body landed, and N is not re-runnable. Only reachable when the re-run
//     fails with SQLite's "already exists" / "duplicate column name" — which
//     is itself proof the body committed, since the body is transactional and
//     a partial application would have rolled back. Mark N applied and carry
//     on with the rest.
//
// Anything else is a migration failing deterministically on the data — a
// UNIQUE index the existing rows violate, say. That re-fails identically
// forever, and an unguarded loop would clear, re-fail, and re-dirty on every
// boot, turning a one-time manual recovery into a permanent one. So the
// UNDERLYING migration error is returned rather than the dirty flag: that
// error is the actionable one, and dirty is left set.
func upWithRecovery(m *migrate.Migrate) error {
	err := m.Up()
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	var dirty migrate.ErrDirty
	if !errors.As(err, &dirty) {
		return err
	}
	log.Warn().Int("version", dirty.Version).
		Msg("interrupted migration detected; attempting recovery")

	// Force rejects 0, and read() would then look for a nonexistent migration
	// version 0, so a dirty FIRST migration rewinds to NilVersion instead.
	prev := dirty.Version - 1
	if prev < 1 {
		prev = database.NilVersion
	}
	if err := m.Force(prev); err != nil {
		return fmt.Errorf("recover dirty version %d: force %d: %w", dirty.Version, prev, err)
	}

	rerunErr := m.Up()
	if rerunErr == nil || errors.Is(rerunErr, migrate.ErrNoChange) {
		log.Warn().Int("version", dirty.Version).
			Msg("recovered from interrupted migration (re-applied)")
		return nil
	}
	if !alreadyApplied(rerunErr) {
		return fmt.Errorf("re-applying migration %d after interruption: %w", dirty.Version, rerunErr)
	}

	// The re-run collided with its own committed effects. Record N as applied
	// — the failed re-run re-dirtied it — and finish the remaining migrations.
	if err := m.Force(dirty.Version); err != nil {
		return fmt.Errorf("recover dirty version %d: force %d: %w", dirty.Version, dirty.Version, err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("continuing past recovered migration %d: %w", dirty.Version, err)
	}
	log.Warn().Int("version", dirty.Version).
		Msg("recovered from interrupted migration (body had already committed)")
	return nil
}

// alreadyApplied reports whether err is SQLite complaining that an object the
// migration creates is already there — proof the body committed before the
// crash, since a partially-applied body would have rolled back.
func alreadyApplied(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate column name")
}

func newMigrator(db *sql.DB) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: iofs source: %w", err)
	}
	driver, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrate: sqlite3 driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		return nil, fmt.Errorf("migrate: new instance: %w", err)
	}
	return m, nil
}
