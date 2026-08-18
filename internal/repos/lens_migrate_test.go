package repos

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	storemigrate "knomit/internal/store/migrate"
)

// legacyLensDDL is the shape every home has after running `migrate-registry`
// but before this change: lens MEMBERSHIP is already uid-keyed (write_uid /
// repo_uid point at repos(uid)), but the `lenses` row itself is still keyed by
// name, with no uid column of its own.
//
// Pasted literally rather than derived from the baseline migration: if it
// referenced the live DDL, this test would stop testing the migration the
// instant that DDL changed — which is exactly the regression this whole file
// guards against.
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

// legacyRead is one read mount to seed, with its branch/source carried
// through explicitly rather than always defaulting empty — a migration whose
// copy `SELECT` list silently dropped `branch` or `source` would still pass a
// test where every seeded mount has empty/NULL values for both.
type legacyRead struct {
	repoUID string
	branch  string
	source  string // "" seeds NULL, matching how LensRead.Source round-trips elsewhere
}

// plainRead is a read mount with no non-default branch/source, for the common
// case where only the repo uid matters to the test.
func plainRead(repoUID string) legacyRead { return legacyRead{repoUID: repoUID} }

// legacyLens is one row (plus its read mounts) to seed into a control.db built
// in the legacy shape.
type legacyLens struct {
	name     string
	writeUID string
	reads    []legacyRead
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
		for _, rd := range l.reads {
			var source any
			if rd.source != "" {
				source = rd.source
			}
			_, err := db.Exec(
				`INSERT INTO lens_reads (lens_name, repo_uid, branch, source) VALUES (?, ?, ?, ?)`,
				l.name, rd.repoUID, rd.branch, source)
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
		{name: "eng", writeUID: "repoA", reads: []legacyRead{
			{repoUID: "repoA", branch: "feature/eng", source: "src://eng"},
			plainRead("repoB"),
		}},
		{name: "ops", writeUID: "repoB", reads: []legacyRead{plainRead("repoB")}},
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
	byRepoUID := map[string]LensRead{}
	for _, rd := range eng.Reads {
		byRepoUID[rd.RepoUID] = rd
	}
	repoA, ok := byRepoUID["repoA"]
	require.True(t, ok, "eng's read mount on repoA must survive")
	require.Equal(t, "feature/eng", repoA.Branch, "a non-empty branch must survive the copy")
	require.Equal(t, "src://eng", repoA.Source, "a non-NULL source must survive the copy")
	repoB, ok := byRepoUID["repoB"]
	require.True(t, ok, "eng's read mount on repoB must survive")
	require.Equal(t, "", repoB.Branch)
	require.Equal(t, "", repoB.Source)

	ops, ok, err := r.Get("ops")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, eng.UID, ops.UID, "uids are distinct per lens")
	require.Len(t, ops.Reads, 1)
	require.Equal(t, "repoB", ops.Reads[0].RepoUID)
}

// A lens_reads row whose lens_name matches no lens cannot be carried across:
// the copy is driven by the lens list, and the new lens_reads has a foreign key
// to lenses(uid) that would reject it anyway. Dropping it is correct. Dropping
// it SILENTLY is not — read mounts vanishing with no trace is what an operator
// finds months later as "that lens used to see more repos".
//
// The orphan is seeded with foreign_keys OFF because that is exactly how a real
// one gets there: the legacy schema's FK is only enforced when the pragma is
// on, and the sqlite3 CLI defaults it off.
func TestOpenLensRegistry_UpgradeLogsDiscardedOrphanReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{
		{name: "eng", writeUID: "repoA", reads: []legacyRead{plainRead("repoA")}},
	})

	// Two orphans, so the assertion is on the COUNT and not merely on the
	// warning firing at all.
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=off&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = db.Exec(
		`INSERT INTO lens_reads (lens_name, repo_uid, branch, source) VALUES
		   ('ghost', 'repoA', '', NULL),
		   ('ghost', 'repoB', '', NULL)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = orig })

	r, err := OpenLensRegistry(path)
	require.NoError(t, err, "an orphan must not fail the migration")
	defer r.Close()

	// The surviving lens is untouched.
	eng, ok, err := r.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, eng.Reads, 1)

	logged := buf.String()
	require.Contains(t, logged, `"discarded":2`,
		"the WARN must name how many read mounts were discarded; got %s", logged)
	require.Contains(t, logged, "dropped read mounts",
		"the WARN must say what happened; got %s", logged)
}

// The mirror of the test above: a clean migration must NOT cry wolf. A count
// comparison that was off by one — or that fired unconditionally — would train
// operators to ignore the one warning that matters.
func TestOpenLensRegistry_UpgradeIsSilentWithNoOrphans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{
		{name: "eng", writeUID: "repoA", reads: []legacyRead{plainRead("repoA"), plainRead("repoB")}},
		{name: "ops", writeUID: "repoB", reads: []legacyRead{plainRead("repoB")}},
	})

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = orig })

	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer r.Close()

	require.NotContains(t, buf.String(), "dropped read mounts",
		"a migration that discarded nothing must warn about nothing")
}

// The upgrade must be idempotent — Start opens this registry on every boot.
func TestOpenLensRegistry_UpgradeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{{name: "eng", writeUID: "repoA", reads: []legacyRead{plainRead("repoA")}}})

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

// upgradeLensSchema rebuilds `lenses` and `lens_reads` from a hand-copy of the
// lens DDL (lens_migrate.go), because SQLite cannot re-key a PRIMARY KEY in
// place. That copy is a THIRD statement of a schema whose other two statements
// are the baseline migration and the legacy DDL pinned by
// TestControl_BaselineMatchesLiveShape in internal/store/migrate.
//
// It has to produce exactly what the baseline produces, and nothing else checks
// that. The baseline runs straight after the re-key with IF NOT EXISTS on every
// statement, so it is a silent no-op over whatever the re-key left behind: a
// home that has been through the re-key would carry the hand-copy's shape
// forever, while a fresh home carries the baseline's, with no error at runtime
// to say the two differ.
//
// So compare them: a re-keyed database against one the migrator built from
// scratch.
func TestUpgradeLensSchema_ProducesTheBaselineShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	seedLegacyLenses(t, path, []legacyLens{
		{name: "eng", writeUID: "repoA", reads: []legacyRead{plainRead("repoA")}},
	})
	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })

	fresh := openControlDB(t, filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, storemigrate.Control(fresh))

	for _, table := range []string{"lenses", "lens_reads"} {
		require.Equal(t, lensTableShape(t, fresh, table), lensTableShape(t, r.db, table),
			"upgradeLensSchema's hand-copy of the %q DDL has drifted from the baseline\n"+
				"migration it is supposed to reproduce (internal/store/migrate/control/\n"+
				"000001_control_baseline.up.sql). The baseline's IF NOT EXISTS cannot correct\n"+
				"it: every re-keyed home would carry this shape permanently, and every fresh\n"+
				"home the baseline's, with nothing at runtime to say so. Bring lens_migrate.go\n"+
				"back in line, or migrate the difference deliberately.", table)
	}
}

// lensTableShape renders the observable shape of table: columns, foreign keys
// and indexes, including the CREATE text of each explicit index.
//
// A deliberate near-duplicate of requireSameTableShape's helpers in
// internal/store/migrate/control_test.go — cross-package test helpers cannot be
// shared, and internal/store/migrate cannot import internal/repos anyway (that
// is the direction of the dependency). Comments in both places point at the
// other; the two must stay comparable in strictness.
func lensTableShape(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	out := pragmaRowStrings(t, db, "pragma_table_info", table)
	out = append(out, pragmaRowStrings(t, db, "pragma_foreign_key_list", table)...)

	names := []string{}
	props := map[string]string{}
	rows, err := db.Query(`SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, table)
	require.NoError(t, err)
	for rows.Next() {
		var name, origin string
		var unique, partial int
		require.NoError(t, rows.Scan(&name, &unique, &origin, &partial))
		names = append(names, name)
		props[name] = fmt.Sprintf("unique=%d origin=%s partial=%d", unique, origin, partial)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	sort.Strings(names)

	for _, name := range names {
		var ddl sql.NullString
		require.NoError(t, db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&ddl))
		// SQLite strips IF NOT EXISTS from what it stores but keeps the author's
		// whitespace, so the two spellings only compare equal once collapsed.
		text := "<implicit>"
		if ddl.Valid {
			text = strings.Join(strings.Fields(ddl.String), " ")
		}
		out = append(out, fmt.Sprintf("%s %s sql=%s cols=%v",
			name, props[name], text, pragmaRowStrings(t, db, "pragma_index_info", name)))
	}
	return out
}

// pragmaRowStrings renders every row of a single-argument table-valued PRAGMA
// as "col=value col=value ...", in the PRAGMA's own (meaningful) row order.
func pragmaRowStrings(t *testing.T, db *sql.DB, pragma, arg string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM `+pragma+`(?)`, arg)
	require.NoError(t, err)
	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)

	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))

		var b strings.Builder
		for i, col := range cols {
			if i > 0 {
				b.WriteByte(' ')
			}
			v := "NULL"
			if vals[i].Valid {
				v = vals[i].String
			}
			fmt.Fprintf(&b, "%s=%s", col, v)
		}
		out = append(out, b.String())
	}
	require.NoError(t, rows.Err())
	return out
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
