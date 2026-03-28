package migrate_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/store/migrate"
)

func TestCore_CreatesStandardTables(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate.Core(db); err != nil {
		t.Fatalf("Core: %v", err)
	}

	// Standard tables should exist.
	for _, table := range []string{"objects", "refs", "kv", "facts", "commit_log", "remotes"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found after Core: %v", table, err)
		}
	}
}

func TestCore_IsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate.Core(db); err != nil {
		t.Fatalf("first Core: %v", err)
	}
	if err := migrate.Core(db); err != nil {
		t.Fatalf("second Core (idempotency): %v", err)
	}
}

func TestCore_DoesNotCreateVec0Table(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate.Core(db); err != nil {
		t.Fatalf("Core: %v", err)
	}

	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='facts_vec'`).Scan(&name)
	if err == nil {
		t.Error("Core should NOT create facts_vec — that requires the vec0 extension")
	}
}
