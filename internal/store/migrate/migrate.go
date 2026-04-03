package migrate

import (
	"database/sql"
	"embed"
	"fmt"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Core applies only the standard SQLite migrations (version 1: all base tables).
// Works with the plain "sqlite3" driver — no extensions required.
// Used by storegit.NewMemoryStorer and internal/git tests.
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

// All applies all migrations including vec0 and GraphQLite (versions 1–3).
// db must be opened with the "sqlite3_knomit" driver (extensions loaded).
// Called by store.Open.
func All(db *sql.DB) error {
	m, err := newMigrator(db)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate.All: %w", err)
	}
	return nil
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
