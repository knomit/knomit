package migrate

import (
	"database/sql"
	"embed"
	"fmt"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/control/*.sql
var controlMigrationsFS embed.FS

// Control applies the control-plane migration set to control.db.
//
// This is a SEPARATE set from All/Core: control.db is a different file with its
// own schema_migrations table, so the two never interact. It uses the stock
// "sqlite3" driver — control.db holds no vectors and needs no extensions.
//
// Like All, it recovers ONCE from a dirty version. That matters more here than
// for repo DBs: control.db is migrated during bootstrap on a possibly-empty
// disk, and a wedged migration would fail every subsequent container start.
func Control(db *sql.DB) error {
	m, err := newControlMigrator(db)
	if err != nil {
		return err
	}
	if err := upWithRecovery(m); err != nil {
		return fmt.Errorf("migrate.Control: %w", err)
	}
	return nil
}

func newControlMigrator(db *sql.DB) (*migrate.Migrate, error) {
	src, err := iofs.New(controlMigrationsFS, "migrations/control")
	if err != nil {
		return nil, fmt.Errorf("migrate.Control: iofs source: %w", err)
	}
	driver, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrate.Control: sqlite3 driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", driver)
	if err != nil {
		return nil, fmt.Errorf("migrate.Control: new instance: %w", err)
	}
	return m, nil
}
