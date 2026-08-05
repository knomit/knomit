package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
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

// legacyControlSchema is the control.db schema EXACTLY as the pre-migration
// code created it: the `lensSchema` const from internal/repos/lens.go (with the
// description column its runtime ALTER had already bolted on) and the
// `repoSettingsSchema` const from internal/repos/settings.go. Nothing here is
// invented — this is the shape every install that predates versioned control.db
// migrations actually has on disk, which is the only shape worth testing the
// adoption against.
const legacyControlSchema = `
CREATE TABLE IF NOT EXISTS lenses (
    name        TEXT PRIMARY KEY,
    write_repo  TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);
CREATE TABLE IF NOT EXISTS repo_settings (
    repo_id TEXT PRIMARY KEY,
    profile TEXT NOT NULL
);
`

// A control.db created by the old inline CREATE TABLE IF NOT EXISTS path has
// the tables and real data in them, but no schema_migrations. Control must
// ADOPT it — migration 1 is IF NOT EXISTS so it no-ops, and migration 2's
// ALTER TABLE … ADD COLUMN description collides with the column that is already
// there, which upWithRecovery's alreadyApplied path must read as "applied"
// rather than as a failure. That collision is the whole point of this test: it
// is the versioned equivalent of the "duplicate column name" error the old
// runtime code swallowed by hand.
//
// This is the migration standing between every existing install and its data,
// so the fixture is the verbatim legacy DDL (see legacyControlSchema) with rows
// in every table, including a lens_reads row whose foreign key only resolves if
// lenses kept its real primary key.
func TestControlAdoptsPreMigrationDB(t *testing.T) {
	db := openControl(t, t.TempDir())
	if _, err := db.Exec(legacyControlSchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	_, err := db.Exec(`
		INSERT INTO lenses (name, write_repo, description, created_at, updated_at)
			VALUES ('eng', 'core', 'engineering knowledge', 1700000000, 1700000001);
		INSERT INTO lens_reads (lens_name, repo, branch, source)
			VALUES ('eng', 'core', 'main', 'core-src');
		INSERT INTO repo_settings (repo_id, profile) VALUES ('core', 'code');
	`)
	if err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}

	if err := Control(db); err != nil {
		t.Fatalf("Control over legacy DB: %v", err)
	}

	// The lens survives with its description intact. Reading the column back is
	// what proves migration 2 was adopted rather than re-run: a re-run would
	// have failed the migration outright, and a migration that "succeeded" by
	// recreating the table would have dropped this row.
	var writeRepo, description string
	if err := db.QueryRow(`SELECT write_repo, description FROM lenses WHERE name='eng'`).
		Scan(&writeRepo, &description); err != nil {
		t.Fatalf("pre-existing lens lost: %v", err)
	}
	if writeRepo != "core" || description != "engineering knowledge" {
		t.Errorf("lens = (%q, %q), want (%q, %q)", writeRepo, description, "core", "engineering knowledge")
	}

	var source string
	if err := db.QueryRow(`SELECT source FROM lens_reads WHERE lens_name='eng' AND repo='core'`).
		Scan(&source); err != nil {
		t.Fatalf("pre-existing lens_read lost: %v", err)
	}
	if source != "core-src" {
		t.Errorf("lens_read source = %q, want %q", source, "core-src")
	}

	var profile string
	if err := db.QueryRow(`SELECT profile FROM repo_settings WHERE repo_id='core'`).Scan(&profile); err != nil {
		t.Fatalf("pre-existing row lost: %v", err)
	}
	if profile != "code" {
		t.Errorf("profile = %q, want %q", profile, "code")
	}

	// The tables this PR adds are there too — adoption is an upgrade, not a
	// stop-at-the-tables-that-already-existed.
	for _, tbl := range []string{"repos", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %q missing after adopting a legacy control.db", tbl)
		}
	}

	// Writing a lens the ordinary way must still work afterwards: the FK on
	// lens_reads only resolves if migration 1 left the legacy lenses table (and
	// its `name` primary key) alone rather than shadowing it.
	if _, err := db.Exec(`
		INSERT INTO lenses (name, write_repo, description, created_at, updated_at)
			VALUES ('ops', 'core', '', 1700000002, 1700000002);
		INSERT INTO lens_reads (lens_name, repo, branch, source) VALUES ('ops', 'core', '', NULL);
	`); err != nil {
		t.Fatalf("adopted schema rejects an ordinary lens write: %v", err)
	}
}

// TestControlRejectsUnknownRepoState pins the CHECK constraint on repos.state.
//
// A row holding anything other than 'active' or 'archived' matches NEITHER
// List(RepoActive) nor List(RepoArchived), so the repo and its database would
// be missing from every surface with nothing logged — the one way left for a
// repo to disappear quietly, which is what this whole registry exists to
// prevent. The constraint turns that into a loud write failure.
func TestControlRejectsUnknownRepoState(t *testing.T) {
	db := openControl(t, t.TempDir())
	if err := Control(db); err != nil {
		t.Fatalf("Control: %v", err)
	}

	for _, state := range []string{"active", "archived"} {
		if _, err := db.Exec(
			`INSERT INTO repos (name, state, archive_id) VALUES (?, ?, ?)`,
			"repo-"+state, state, state,
		); err != nil {
			t.Fatalf("state %q must be accepted: %v", state, err)
		}
	}

	if _, err := db.Exec(
		`INSERT INTO repos (name, state, archive_id) VALUES ('ghost', 'retired', '')`,
	); err == nil {
		t.Error("an unknown state was accepted; such a row is invisible to both List filters")
	}
	if _, err := db.Exec(`UPDATE repos SET state = 'retired' WHERE name = 'repo-active'`); err == nil {
		t.Error("an unknown state was accepted on UPDATE")
	}
}

// TestControlRepairsUnknownRepoStateOnUpgrade covers the other half of
// migration 5: a control.db that ALREADY carries a bad state — written before
// the constraint existed — must be repaired, not rejected. Failing the
// migration would take the whole instance offline over one bad row, which is
// the opposite of what the boot reconcile does everywhere else.
//
// archive_id is what says which state the row meant: active rows carry ”,
// archived rows carry their id.
func TestControlRepairsUnknownRepoStateOnUpgrade(t *testing.T) {
	db := openControl(t, t.TempDir())
	// Stop one migration short of the constraint, so the bad rows can be
	// written the way a pre-constraint install would have written them.
	m, err := newControlMigrator(db)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO repos (name, state, archive_id) VALUES ('live', 'retired', '');
		INSERT INTO repos (name, state, archive_id) VALUES ('old', 'retired', 'arc-1');
	`); err != nil {
		t.Fatalf("seed bad states: %v", err)
	}

	if err := Control(db); err != nil {
		t.Fatalf("Control must repair bad states, not fail: %v", err)
	}

	for _, tc := range []struct{ name, want string }{
		{"live", "active"},
		{"old", "archived"},
	} {
		var got string
		if err := db.QueryRow(`SELECT state FROM repos WHERE name = ?`, tc.name).Scan(&got); err != nil {
			t.Fatalf("row %q lost by the repair: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("repo %q state = %q, want %q (archive_id decides)", tc.name, got, tc.want)
		}
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

// TestControlAddsOriginAuthColumnsToPreexistingRows verifies that migration 6
// applies the auth columns to existing repos rows, filling them with the empty-string
// defaults so pre-existing rows survive the ALTER without rewriting.
func TestControlAddsOriginAuthColumnsToPreexistingRows(t *testing.T) {
	db := openControl(t, t.TempDir())

	// Migrate to version 5, stopping short of migration 6 so we can insert a row
	// the way pre-migration-6 code would have.
	m, err := newControlMigrator(db)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(5))

	// Insert a row that predates migration 6.
	_, err = db.Exec(`INSERT INTO repos (name, archive_id, state) VALUES ('work', '', 'active')`)
	require.NoError(t, err)

	// Now apply migration 6 by running Control (which migrates to the latest).
	require.NoError(t, Control(db))

	// The pre-existing row must have the auth columns with their empty-string defaults.
	var method, token string
	require.NoError(t, db.QueryRow(
		`SELECT auth_method, auth_token FROM repos WHERE name='work'`).
		Scan(&method, &token))
	require.Equal(t, "", method, "auth_method must default to empty string")
	require.Equal(t, "", token, "auth_token must default to empty string")
}
