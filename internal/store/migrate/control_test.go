package migrate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// controlDB opens a temp control.db with the same DSN internal/repos uses.
func controlDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func objectExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n))
	return n > 0
}

func controlVersion(t *testing.T, db *sql.DB) (int, bool) {
	t.Helper()
	var v int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty))
	return v, dirty
}

// The five objects the baseline is responsible for.
var controlObjects = []string{
	"repos", "repos_active_name", "repos_active_repo_id",
	"repo_origins", "lenses", "lenses_name", "lens_reads",
}

// A fresh home gets the whole control schema and lands on version 1.
func TestControl_FreshDatabase(t *testing.T) {
	db := controlDB(t)
	require.NoError(t, Control(db))

	for _, name := range controlObjects {
		require.True(t, objectExists(t, db, name), "expected %q to exist", name)
	}
	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)
}

// An existing home already carrying the current shape is stamped, not rebuilt:
// the baseline is a no-op and every row survives.
func TestControl_NoOpOnExistingShape(t *testing.T) {
	db := controlDB(t)

	// Build the current shape by hand, the way a pre-migrate home looks.
	_, err := db.Exec(`
CREATE TABLE repos (
    uid TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'code', repo_id TEXT,
    created_at INTEGER NOT NULL, archived_at INTEGER);
CREATE UNIQUE INDEX repos_active_name ON repos(name) WHERE state = 'active';
CREATE UNIQUE INDEX repos_active_repo_id ON repos(repo_id) WHERE state = 'active' AND repo_id IS NOT NULL;
CREATE TABLE repo_origins (
    repo_uid TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    url TEXT NOT NULL, branch TEXT NOT NULL,
    auth_method TEXT NOT NULL DEFAULT '', auth_token TEXT NOT NULL DEFAULT '');
CREATE TABLE lenses (
    uid TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL,
    write_uid TEXT NOT NULL REFERENCES repos(uid),
    description TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX lenses_name ON lenses(name);
CREATE TABLE lens_reads (
    lens_uid TEXT NOT NULL REFERENCES lenses(uid) ON DELETE CASCADE,
    repo_uid TEXT NOT NULL REFERENCES repos(uid),
    branch TEXT NOT NULL DEFAULT '', source TEXT,
    PRIMARY KEY (lens_uid, repo_uid));`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO repos (uid, name, state, profile, created_at)
	                  VALUES ('u1', 'alpha', 'active', 'code', 7)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO lenses (uid, name, write_uid, created_at, updated_at)
	                  VALUES ('l1', 'work', 'u1', 7, 7)`)
	require.NoError(t, err)

	require.NoError(t, Control(db))

	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)

	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM repos WHERE uid = 'u1'`).Scan(&name))
	require.Equal(t, "alpha", name)
	var lens string
	require.NoError(t, db.QueryRow(`SELECT name FROM lenses WHERE uid = 'l1'`).Scan(&lens))
	require.Equal(t, "work", lens)
}

// legacyControlDDL is the shape every control.db that predates this migration
// already carries on disk: the verbatim text of the three ad-hoc
// `CREATE TABLE IF NOT EXISTS` constants internal/repos ran on every open
// before control.db was versioned — registrySchema (internal/repos/registry.go),
// originsSchema (internal/repos/origins.go) and lensSchema
// (internal/repos/lens.go), in dependency order. The constants themselves were
// deleted when the baseline migration replaced them; recover the originals with
// `git show 050def4b:internal/repos/registry.go` and friends.
//
// Pasted literally rather than derived from control/000001_control_baseline.up.sql,
// for exactly the reason legacyLensDDL in internal/repos/lens_migrate_test.go is:
// an INDEPENDENT copy is the whole point. Generate it from the baseline and the
// two agree by construction, and TestControl_BaselineMatchesLiveShape below
// asserts nothing at all. Keeping the two in step is meant to be a deliberate
// act — see that test for what to do when it fails.
const legacyControlDDL = `
CREATE TABLE IF NOT EXISTS repos (
    uid         TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    state       TEXT NOT NULL,
    profile     TEXT NOT NULL DEFAULT 'code',
    repo_id     TEXT,
    created_at  INTEGER NOT NULL,
    archived_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_name
    ON repos(name) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_repo_id
    ON repos(repo_id) WHERE state = 'active' AND repo_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS repo_origins (
    repo_uid    TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    branch      TEXT NOT NULL,
    auth_method TEXT NOT NULL DEFAULT '',
    auth_token  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS lenses (
    uid         TEXT PRIMARY KEY NOT NULL,
    name        TEXT NOT NULL,
    write_uid   TEXT NOT NULL REFERENCES repos(uid),
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS lenses_name ON lenses(name);
CREATE TABLE IF NOT EXISTS lens_reads (
    lens_uid  TEXT NOT NULL REFERENCES lenses(uid) ON DELETE CASCADE,
    repo_uid  TEXT NOT NULL REFERENCES repos(uid),
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_uid, repo_uid)
);`

// controlTables are the four tables the baseline is responsible for, in
// dependency order.
var controlTables = []string{"repos", "repo_origins", "lenses", "lens_reads"}

// The baseline migration is a RESTATEMENT of DDL that already exists in every
// live home, not a fresh design. If it drifts, fresh homes get a different
// control.db shape from existing ones — permanently, and with no runtime signal
// whatsoever, because every baseline statement is IF NOT EXISTS and therefore a
// silent no-op against a table that is already there. Neither
// TestControl_FreshDatabase (object NAMES only) nor
// TestControl_NoOpOnExistingShape (the no-op is the point) can see that.
//
// So compare the two shapes directly: database A built by hand from the legacy
// DDL, database B built by the migrator.
func TestControl_BaselineMatchesLiveShape(t *testing.T) {
	live := controlDB(t)
	_, err := live.Exec(legacyControlDDL)
	require.NoError(t, err)

	fresh := controlDB(t)
	require.NoError(t, Control(fresh))

	for _, table := range controlTables {
		requireSameTableShape(t, live, fresh, table)
	}
}

// requireSameTableShape fails unless table has an identical observable shape in
// want (a database carrying the legacy DDL) and got (a database built by the
// migrator): same columns, same foreign keys, same indexes — including the
// WHERE predicate of a partial index, which PRAGMA index_list reports only as a
// boolean.
func requireSameTableShape(t *testing.T, want, got *sql.DB, table string) {
	t.Helper()
	const drift = "control.db baseline has DRIFTED from the shape live databases carry.\n" +
		"internal/store/migrate/control/000001_control_baseline.up.sql is a restatement of DDL\n" +
		"that already exists on disk in every existing home, and its statements are all\n" +
		"IF NOT EXISTS — so this difference in %q would NEVER be applied to an existing\n" +
		"home. Fresh homes and existing homes would carry different schemas forever, with\n" +
		"no error at runtime. Either restore the baseline to match the legacy DDL, or add\n" +
		"a NEW numbered migration that moves existing homes to the new shape.\n" +
		"(%s)"

	require.Equal(t, tableColumns(t, want, table), tableColumns(t, got, table),
		drift, table, "PRAGMA table_info")
	require.Equal(t, tableForeignKeys(t, want, table), tableForeignKeys(t, got, table),
		drift, table, "PRAGMA foreign_key_list")
	require.Equal(t, tableIndexes(t, want, table), tableIndexes(t, got, table),
		drift, table, "PRAGMA index_list / index_info / sqlite_master.sql")
}

// tableColumns renders PRAGMA table_info in declaration order: name, type,
// notnull, dflt_value and pk, which together are what makes a column what it is.
func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return pragmaRows(t, db, "pragma_table_info", table)
}

// tableForeignKeys renders PRAGMA foreign_key_list — the parent table, the
// column pair and the ON UPDATE / ON DELETE actions. table_info cannot see any
// of it, so a lost `ON DELETE CASCADE` would slip past a column-only comparison.
func tableForeignKeys(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return pragmaRows(t, db, "pragma_foreign_key_list", table)
}

// tableIndexes renders every index on table, sorted by name so creation order
// cannot make two identical schemas compare unequal.
//
// PRAGMA index_list reports `partial` as a bare 0/1 and never exposes the WHERE
// clause, so repos_active_name and repos_active_repo_id would compare equal even
// if one database indexed a completely different subset of rows. The index's own
// CREATE text from sqlite_master is what pins the predicate; SQLite strips
// IF NOT EXISTS from what it stores but preserves the author's whitespace, so it
// is compared with whitespace collapsed.
func tableIndexes(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
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

	out := make([]string, 0, len(names))
	for _, name := range names {
		var ddl sql.NullString
		require.NoError(t, db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&ddl))
		// Auto-indexes (PRIMARY KEY, UNIQUE) have a NULL sql; their shape is
		// carried entirely by index_info below.
		text := "<implicit>"
		if ddl.Valid {
			text = strings.Join(strings.Fields(ddl.String), " ")
		}
		out = append(out, fmt.Sprintf("%s %s sql=%s cols=%v",
			name, props[name], text, pragmaRows(t, db, "pragma_index_info", name)))
	}
	return out
}

// pragmaRows renders every row of a single-argument table-valued PRAGMA as
// "col=value col=value ...", preserving the PRAGMA's own row order (which is
// meaningful: column position in a table, column position within an index).
func pragmaRows(t *testing.T, db *sql.DB, pragma, arg string) []string {
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

// An interrupted control.db migration self-heals rather than wedging every
// repo behind a permanently dirty control plane (issue #33).
func TestControl_RecoversDirtyVersion(t *testing.T) {
	db := controlDB(t)
	require.NoError(t, Control(db))

	_, err := db.Exec(`UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, Control(db), "a dirty control.db must self-heal")

	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)
	require.True(t, objectExists(t, db, "repos"))
}
