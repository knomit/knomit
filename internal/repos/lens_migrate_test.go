package repos

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyLensDDL is the shape every home has after running `migrate-registry`
// but before this change: lens MEMBERSHIP is already uid-keyed (write_uid /
// repo_uid point at repos(uid)), but the `lenses` row itself is still keyed by
// name, with no uid column of its own.
//
// Pasted literally rather than referencing lensSchema: if it referenced the
// constant, this test would stop testing the migration the instant the
// constant changed — which is exactly the regression this whole file guards
// against.
const legacyLensDDL = `
CREATE TABLE lenses (
    name        TEXT PRIMARY KEY,
    write_uid   TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo_uid  TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo_uid)
);`

// legacyLens is one row (plus its read mounts) to seed into a control.db built
// in the legacy shape.
type legacyLens struct {
	name     string
	writeUID string
	reads    []string // repo uids mounted as reads; writeUID is not implied here
}

// seedLegacyLenses opens path with the same DSN OpenLensRegistry uses, execs
// legacyLensDDL literally, and inserts the given rows. It does NOT go through
// OpenLensRegistry, so nothing upgrades the shape before the test does.
func seedLegacyLenses(t *testing.T, path string, lenses []legacyLens) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(legacyLensDDL)
	require.NoError(t, err)

	for _, l := range lenses {
		_, err := db.Exec(
			`INSERT INTO lenses (name, write_uid, description, created_at, updated_at) VALUES (?, ?, '', 1, 1)`,
			l.name, l.writeUID)
		require.NoError(t, err)
		for _, repoUID := range l.reads {
			_, err := db.Exec(
				`INSERT INTO lens_reads (lens_name, repo_uid, branch, source) VALUES (?, ?, '', NULL)`,
				l.name, repoUID)
			require.NoError(t, err)
		}
	}
}

// A legacy control.db must come out of OpenLensRegistry in the new shape with
// every lens and every read mount intact. This is the case that silently
// no-ops if the upgrade is attempted with CREATE TABLE IF NOT EXISTS.
func TestOpenLensRegistry_UpgradesLegacyNameKeyedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{
		{name: "eng", writeUID: "repoA", reads: []string{"repoA", "repoB"}},
		{name: "ops", writeUID: "repoB", reads: []string{"repoB"}},
	})

	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer r.Close()

	eng, ok, err := r.Get("eng")
	require.NoError(t, err)
	require.True(t, ok, "the lens must survive the upgrade")
	require.NotEmpty(t, eng.UID, "every migrated lens gets a uid")
	require.Equal(t, "repoA", eng.WriteUID)
	require.Len(t, eng.Reads, 2, "read mounts must be carried across")

	// The read mounts themselves, not just their count: this is the case a
	// migration that carried lenses across but dropped lens_reads would still
	// pass a careless "does the lens exist" check.
	gotRepoUIDs := map[string]bool{}
	for _, rd := range eng.Reads {
		gotRepoUIDs[rd.RepoUID] = true
	}
	require.True(t, gotRepoUIDs["repoA"], "eng's read mount on repoA must survive")
	require.True(t, gotRepoUIDs["repoB"], "eng's read mount on repoB must survive")

	ops, ok, err := r.Get("ops")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, eng.UID, ops.UID, "uids are distinct per lens")
	require.Len(t, ops.Reads, 1)
	require.Equal(t, "repoB", ops.Reads[0].RepoUID)
}

// The upgrade must be idempotent — Start opens this registry on every boot.
func TestOpenLensRegistry_UpgradeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{{name: "eng", writeUID: "repoA", reads: []string{"repoA"}}})

	r1, err := OpenLensRegistry(path)
	require.NoError(t, err)
	first, ok, err := r1.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r1.Close())

	r2, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer r2.Close()
	second, ok, err := r2.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.UID, second.UID, "a second open must not re-mint the uid")
	require.Len(t, second.Reads, 1, "read mounts survive a second open too")
}

// A fresh database goes straight to the new shape with no upgrade step.
func TestOpenLensRegistry_FreshDBIsAlreadyNewShape(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	repoUID := seedMember(t, repoReg, "repoA")

	got, err := r.Create(Lens{Name: "eng", WriteUID: repoUID,
		CreatedAt: 1, UpdatedAt: 1,
		Reads: []LensRead{{RepoUID: repoUID}}})
	require.NoError(t, err)
	require.NotEmpty(t, got.UID)

	fetched, ok, err := r.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, got.UID, fetched.UID)
}
