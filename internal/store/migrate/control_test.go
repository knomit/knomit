package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openControl(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "control.db")+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// tableExists is defined in recovery_test.go (same package).

func TestControlFreshCreate(t *testing.T) {
	db := openControl(t, t.TempDir())
	if err := Control(db); err != nil {
		t.Fatalf("Control: %v", err)
	}
	for _, tbl := range []string{"lenses", "lens_reads", "repo_settings", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %q missing after Control", tbl)
		}
	}
}

// A control.db created by the old inline CREATE TABLE IF NOT EXISTS path has
// the tables but no schema_migrations. Control must adopt it, not fail.
func TestControlAdoptsPreMigrationDB(t *testing.T) {
	db := openControl(t, t.TempDir())
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS lenses (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS repo_settings (
			repo_id TEXT PRIMARY KEY,
			profile TEXT NOT NULL
		);
		INSERT INTO repo_settings (repo_id, profile) VALUES ('core', 'code');
	`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	if err := Control(db); err != nil {
		t.Fatalf("Control over legacy DB: %v", err)
	}

	var profile string
	if err := db.QueryRow(`SELECT profile FROM repo_settings WHERE repo_id='core'`).Scan(&profile); err != nil {
		t.Fatalf("pre-existing row lost: %v", err)
	}
	if profile != "code" {
		t.Errorf("profile = %q, want %q", profile, "code")
	}
}

func TestControlIsIdempotent(t *testing.T) {
	db := openControl(t, t.TempDir())
	if err := Control(db); err != nil {
		t.Fatalf("first Control: %v", err)
	}
	if err := Control(db); err != nil {
		t.Fatalf("second Control: %v", err)
	}
}
