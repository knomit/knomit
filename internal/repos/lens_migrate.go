package repos

import (
	"database/sql"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/segmentio/ksuid"
)

// upgradeLensSchema re-keys a name-keyed `lenses`/`lens_reads` pair onto uids.
//
// Guarded by an explicit column probe rather than CREATE TABLE IF NOT EXISTS,
// which is a NO-OP against an existing table — that is exactly how the previous
// lens re-keying could not be done on the open path (see LensSchemaSQL). The
// probe is what makes this work where that would not.
//
// The starting shape this expects is "membership already uid-keyed, lens row
// itself still name-keyed": `lenses.write_uid` / `lens_reads.repo_uid` already
// point at repos(uid) (every home that has ever run `migrate-registry`), but
// `lenses` itself is still keyed by name with no `uid` column of its own. A
// genuinely pre-registry home (`lenses.write_repo`) never reaches here at all:
// Manager.Start's boot guard refuses it before OpenLensRegistry runs, and only
// `migrate-registry` converts that shape.
//
// Runs on every open and is idempotent: a database already carrying lenses.uid
// returns immediately, so uids are never re-minted.
//
// SQLite cannot change a PRIMARY KEY in place, so both tables are rebuilt.
// foreign_keys is disabled for the duration — lens_reads references lenses(name)
// until the swap completes, and the rebuild would trip the constraint mid-flight.
// The PRAGMA is a no-op inside a transaction, so it is issued outside one and
// restored in a defer.
func upgradeLensSchema(db *sql.DB) error {
	hasLenses, err := lensTableExists(db)
	if err != nil {
		return fmt.Errorf("lens upgrade: probe table: %w", err)
	}
	if !hasLenses {
		return nil // fresh database: lensSchema creates the new shape directly
	}
	hasUID, err := lensColumnExists(db, "lenses", "uid")
	if err != nil {
		return fmt.Errorf("lens upgrade: probe column: %w", err)
	}
	if hasUID {
		return nil // already migrated
	}

	if _, err := db.Exec(`PRAGMA foreign_keys=off`); err != nil {
		return fmt.Errorf("lens upgrade: disable fks: %w", err)
	}
	defer func() {
		if _, derr := db.Exec(`PRAGMA foreign_keys=on`); derr != nil {
			log.Error().Err(derr).Msg("lens upgrade: could not restore foreign_keys")
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("lens upgrade: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Mint a uid per existing lens, in Go rather than SQL — ksuid is how repo
	// uids are minted and the two must be indistinguishable in shape.
	rows, err := tx.Query(`SELECT name FROM lenses`)
	if err != nil {
		return fmt.Errorf("lens upgrade: read names: %w", err)
	}
	uidByName := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("lens upgrade: scan name: %w", err)
		}
		uidByName[name] = ksuid.New().String()
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("lens upgrade: rows: %w", err)
	}
	rows.Close()

	stmts := []string{
		`ALTER TABLE lenses RENAME TO lenses_old`,
		`ALTER TABLE lens_reads RENAME TO lens_reads_old`,
		`CREATE TABLE lenses (
		    uid TEXT PRIMARY KEY, name TEXT NOT NULL,
		    write_uid TEXT NOT NULL REFERENCES repos(uid),
		    description TEXT NOT NULL DEFAULT '',
		    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE UNIQUE INDEX lenses_name ON lenses(name)`,
		`CREATE TABLE lens_reads (
		    lens_uid TEXT NOT NULL REFERENCES lenses(uid) ON DELETE CASCADE,
		    repo_uid TEXT NOT NULL REFERENCES repos(uid),
		    branch TEXT NOT NULL DEFAULT '', source TEXT,
		    PRIMARY KEY (lens_uid, repo_uid))`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("lens upgrade: %q: %w", s, err)
		}
	}

	for name, uid := range uidByName {
		if _, err := tx.Exec(
			`INSERT INTO lenses (uid, name, write_uid, description, created_at, updated_at)
			 SELECT ?, name, write_uid, description, created_at, updated_at
			   FROM lenses_old WHERE name = ?`, uid, name); err != nil {
			return fmt.Errorf("lens upgrade: copy lens %q: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO lens_reads (lens_uid, repo_uid, branch, source)
			 SELECT ?, repo_uid, branch, source
			   FROM lens_reads_old WHERE lens_name = ?`, uid, name); err != nil {
			return fmt.Errorf("lens upgrade: copy reads for %q: %w", name, err)
		}
	}

	for _, s := range []string{`DROP TABLE lens_reads_old`, `DROP TABLE lenses_old`} {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("lens upgrade: %q: %w", s, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lens upgrade: commit: %w", err)
	}
	log.Info().Int("lenses", len(uidByName)).Msg("lens registry: re-keyed onto uids")
	return nil
}

// lensTableExists / lensColumnExists mirror migrate-registry's rawTableExists /
// rawColumnExists (cmd/migrate_registry.go:1761,1770) — same sqlite_master /
// PRAGMA table_info shapes. Duplicated rather than shared because cmd/ ->
// internal/ is the only legal import direction; internal/repos cannot import
// cmd's helpers.
func lensTableExists(db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'lenses'`).Scan(&n); err != nil {
		return false, fmt.Errorf("look for table %q: %w", "lenses", err)
	}
	return n > 0, nil
}

func lensColumnExists(db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("look for %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}
