package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/segmentio/ksuid"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// The fixture's agent branch. Fixed rather than host-derived so the rewind and
// the assertions below name the same ref on every machine.
const testAgentBranch = "agent/test"

// The plaintext credential the fixture encrypts. The tool must move the
// CIPHERTEXT and never see this string; the round-trip at the end of
// TestMigrateRegistry_ConvertsALegacyHome is what proves it.
const testAuthToken = "s3cret"

// ---------------------------------------------------------------------------
// the legacy-home fixture
// ---------------------------------------------------------------------------

// legacyRemote is the connection half of a pre-000017 `remotes` row.
type legacyRemote struct {
	url        string
	branch     string
	authMethod string
	authToken  string // already CIPHERTEXT — the fixture never stores plaintext
}

// buildLegacyHome creates a genuinely pre-registry home:
//
//   - repos/alpha.db — a real repo database rewound to schema 16, whose
//     `remotes` row still carries url/branch/auth_method/auth_token, the token
//     being real ciphertext produced by store.NewCrypt from the home's agent
//     key. The tool must copy that ciphertext verbatim, and the test decrypts
//     it at the far end to prove it did.
//   - repos/archive/<ksuid>.db + .json — one archived repo and its manifest.
//   - control.db with the OLD name-keyed lens tables (write_repo / repo) and a
//     repo_settings row keyed by alpha's root commit.
//
// It returns the home path; alphaRootCommit reports the identity the profile
// row is keyed by.
func buildLegacyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(filepath.Join(reposDir, "archive"), 0o755))

	crypt := testCryptFor(t, home)
	cipher, err := crypt.Encrypt(testAuthToken)
	require.NoError(t, err)
	require.NotEqual(t, testAuthToken, cipher, "the fixture must store ciphertext, not plaintext")

	alphaRoot := makeLegacyRepoDB(t,
		filepath.Join(reposDir, "alpha.db"),
		map[string]string{"kb/alpha.md": "alpha"},
		legacyRemote{
			url:        "https://legacy.test/alpha.git",
			branch:     "master",
			authMethod: "token",
			authToken:  cipher,
		})
	// The ephemeral session sidecar a running server would have left behind.
	require.NoError(t, os.WriteFile(
		store.SessionDBPathFor(filepath.Join(reposDir, "alpha.db")), []byte("stale"), 0o600))

	archiveID := ksuid.New().String()
	makeLegacyRepoDB(t,
		filepath.Join(reposDir, "archive", archiveID+".db"),
		map[string]string{"kb/retired.md": "retired"},
		legacyRemote{url: "https://legacy.test/retired.git", branch: "main"})
	writeArchiveManifest(t, filepath.Join(reposDir, "archive", archiveID+".json"), archiveID, "retired",
		"https://legacy.test/retired.git")

	buildLegacyControlDB(t, filepath.Join(home, "control.db"), "alpha", alphaRoot)
	return home
}

// buildLegacyHomeWithTwoClonesOfOneKB makes a home that already violates the
// (new) one-active-repo-per-knowledge-base rule: beta.db is a byte copy of
// alpha.db, so both share a root commit.
func buildLegacyHomeWithTwoClonesOfOneKB(t *testing.T) string {
	t.Helper()
	home := buildLegacyHome(t)
	reposDir := filepath.Join(home, "repos")
	data, err := os.ReadFile(filepath.Join(reposDir, "alpha.db"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(reposDir, "beta.db"), data, 0o600))
	return home
}

// alphaRootCommit re-reads alpha's identity from a legacy home without
// migrating it.
func alphaRootCommit(t *testing.T, home string) string {
	t.Helper()
	db, err := openRaw(filepath.Join(home, "repos", "alpha.db"))
	require.NoError(t, err)
	defer db.Close()
	root, _, err := readRootCommit(db)
	require.NoError(t, err)
	return root
}

// makeLegacyRepoDB creates a repo database at the CURRENT schema, rewinds it
// past migration 000017 with that migration's own down SQL (the technique
// internal/store/remote_migration_test.go uses, so the fixture is the real
// prior schema rather than a hand-built approximation), then writes the
// pre-migration `remotes` row. Returns the repo's root commit.
func makeLegacyRepoDB(t *testing.T, path string, initFiles map[string]string, remote legacyRemote) string {
	t.Helper()
	svc, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(initFiles, testAgentBranch))
	root, err := svc.RootCommit(context.Background(), testAgentBranch)
	require.NoError(t, err)
	require.NoError(t, svc.Close())

	downSQL, err := os.ReadFile(filepath.Join("..", "internal", "store", "migrate", "migrations",
		"000017_remotes_drop_connection.down.sql"))
	require.NoError(t, err)

	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	_, err = raw.Exec(string(downSQL))
	require.NoError(t, err, "000017's down migration must apply to a freshly migrated database")
	_, err = raw.Exec(`UPDATE schema_migrations SET version = 16, dirty = 0`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO remotes (name, url, branch, interval, push_interval, auth_method, auth_token)
		 VALUES ('origin', ?, ?, 300, 300, ?, ?)`,
		remote.url, remote.branch, remote.authMethod, remote.authToken)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	return root
}

func writeArchiveManifest(t *testing.T, path, id, name, origin string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"id":         id,
		"name":       name,
		"origin":     origin,
		"archivedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// buildLegacyControlDB writes the OLD control.db shape: lens tables keyed by
// repo NAME (write_repo / repo) and repo_settings keyed by root commit. There
// is deliberately no repos and no repo_origins table.
func buildLegacyControlDB(t *testing.T, path, writeRepo, rootCommit string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
CREATE TABLE lenses (
    name        TEXT PRIMARY KEY,
    write_repo  TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);
CREATE TABLE repo_settings (
    repo_id TEXT PRIMARY KEY,
    profile TEXT NOT NULL
);`)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO lenses (name, write_repo, description, created_at, updated_at)
		 VALUES ('workspace', ?, 'the legacy lens', 100, 200)`, writeRepo)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO lens_reads (lens_name, repo, branch, source)
		 VALUES ('workspace', ?, 'main', 'src://legacy')`, writeRepo)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO repo_settings (repo_id, profile) VALUES (?, 'chat')`, rootCommit)
	require.NoError(t, err)
}

// testCryptFor returns the Crypt derived from the home's agent key, creating
// the key file on first use. It is the same derivation Manager.Start performs,
// which is the whole point: a token encrypted here must decrypt there.
func testCryptFor(t *testing.T, home string) *store.Crypt {
	t.Helper()
	keyPath := filepath.Join(home, "id_ed25519")
	data, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key-just-key-material\n-----END OPENSSH PRIVATE KEY-----\n")
		require.NoError(t, os.WriteFile(keyPath, data, 0o600))
	} else {
		require.NoError(t, err)
	}
	crypt, err := store.NewCrypt(data)
	require.NoError(t, err)
	return crypt
}

// quietOpts keeps the tool's plan/summary out of the test log.
func quietOpts(o migrateOpts) migrateOpts {
	o.Out = io.Discard
	return o
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestMigrateRegistry_ConvertsALegacyHome(t *testing.T) {
	home := buildLegacyHome(t)
	wantRoot := alphaRootCommit(t, home)
	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{})))

	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	require.False(t, reg.SchemaJustCreated(), "the migrated home must satisfy the boot guard")

	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "alpha", active[0].Name)
	require.Equal(t, "chat", active[0].Profile, "profile carried over by root commit")
	require.Equal(t, wantRoot, active[0].RepoID)
	require.NotEmpty(t, active[0].UID)

	archived, err := reg.List(repos.StateArchived)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, "retired", archived[0].Name)
	require.NotZero(t, archived[0].ArchivedAt)

	// Files are uid-named, the archive directory is gone, and the ephemeral
	// session sidecar went with it.
	require.FileExists(t, filepath.Join(home, "repos", active[0].UID+".db"))
	require.FileExists(t, filepath.Join(home, "repos", archived[0].UID+".db"))
	require.NoFileExists(t, filepath.Join(home, "repos", "alpha.db"))
	require.NoFileExists(t, store.SessionDBPathFor(filepath.Join(home, "repos", "alpha.db")))
	require.NoDirExists(t, filepath.Join(home, "repos", "archive"))
	require.FileExists(t, filepath.Join(home, "control.db.bak"))

	// The credential moved without ever being decrypted, and still decrypts.
	origins, err := repos.OpenOrigins(reg.DB(), testCryptFor(t, home))
	require.NoError(t, err)
	org, err := origins.Get(active[0].UID)
	require.NoError(t, err)
	require.NotNil(t, org)
	require.Equal(t, "https://legacy.test/alpha.git", org.URL)
	require.Equal(t, "master", org.Branch)
	require.Equal(t, "token", org.AuthMethod)
	require.Equal(t, testAuthToken, org.AuthToken)

	// The archived repo's origin came across too, from its OWN remotes row.
	archOrg, err := origins.Get(archived[0].UID)
	require.NoError(t, err)
	require.NotNil(t, archOrg)
	require.Equal(t, "https://legacy.test/retired.git", archOrg.URL)

	// The lens tables were REBUILT in the uid shape, not merely written to.
	requireLensSchemaIsUIDKeyed(t, filepath.Join(home, "control.db"))
	lensReg, err := repos.OpenLensRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer lensReg.Close()
	lens, ok, err := lensReg.Get("workspace")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, active[0].UID, lens.WriteUID, "lens membership re-keyed to the uid")
	require.Equal(t, "the legacy lens", lens.Description)
	require.Len(t, lens.Reads, 1)
	require.Equal(t, active[0].UID, lens.Reads[0].RepoUID)
	require.Equal(t, "main", lens.Reads[0].Branch)
	require.Equal(t, "src://legacy", lens.Reads[0].Source)

	// repo_settings is gone; its one datum lives on the registry row.
	requireTableAbsent(t, filepath.Join(home, "control.db"), "repo_settings")

	// Every repo database moved forward past 000017.
	for _, rec := range append(active, archived...) {
		requireConnectionColumnsDropped(t, filepath.Join(home, "repos", rec.UID+".db"))
	}
}

// A migrated home must boot, and the origin control.db now holds must reach the
// repo's own store the way the server expects it to.
func TestMigrateRegistry_MigratedHomeBootsWithItsOrigin(t *testing.T) {
	home := buildLegacyHome(t)
	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{})))

	cfg := config.Defaults()
	cfg.Home = home
	mgr := repos.New(context.Background(), repos.Deps{
		Cfg:                   cfg,
		AgentBranch:           testAgentBranch,
		KeyPath:               filepath.Join(home, "id_ed25519"),
		DisableBackgroundSync: true,
	})
	require.NoError(t, mgr.Start(), "the boot guard must accept a migrated home")
	defer mgr.Close()

	require.Equal(t, []string{"alpha"}, mgr.Names())
	ri := mgr.Get("alpha")
	require.NotNil(t, ri)

	var got *store.Remote
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		var rerr error
		got, rerr = svc.Remote().GetRemote("origin")
		require.NoError(t, rerr)
	}))
	require.NotNil(t, got, "the origin survived the migration and was injected at boot")
	require.Equal(t, "https://legacy.test/alpha.git", got.URL)
	require.Equal(t, "master", got.Branch)
}

// The identity constraint is new, so a home may already violate it. Refuse,
// having written nothing at all.
func TestMigrateRegistry_RefusesDuplicateIdentities(t *testing.T) {
	home := buildLegacyHomeWithTwoClonesOfOneKB(t)
	err := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "same knowledge base")
	require.Contains(t, err.Error(), "alpha")
	require.Contains(t, err.Error(), "beta")

	// Nothing on disk moved, and no backup was taken.
	require.FileExists(t, filepath.Join(home, "repos", "alpha.db"))
	require.FileExists(t, filepath.Join(home, "repos", "beta.db"))
	require.DirExists(t, filepath.Join(home, "repos", "archive"))
	require.NoFileExists(t, filepath.Join(home, "control.db.bak"))
	requireTableAbsent(t, filepath.Join(home, "control.db"), "repos")

	// --force is about a re-run, not about identity: it must not get past this.
	ferr := runMigrateRegistry(home, quietOpts(migrateOpts{Force: true}))
	require.Error(t, ferr)
	require.Contains(t, ferr.Error(), "same knowledge base")

	reg, oerr := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, oerr)
	defer reg.Close()
	empty, eerr := reg.IsEmpty()
	require.NoError(t, eerr)
	require.True(t, empty, "nothing written")
}

// A dry run reports the plan and changes nothing — not even the backup.
func TestMigrateRegistry_DryRunWritesNothing(t *testing.T) {
	home := buildLegacyHome(t)
	var buf writerRecorder
	require.NoError(t, runMigrateRegistry(home, migrateOpts{DryRun: true, Out: &buf}))

	require.FileExists(t, filepath.Join(home, "repos", "alpha.db"))
	require.DirExists(t, filepath.Join(home, "repos", "archive"))
	require.NoFileExists(t, filepath.Join(home, "control.db.bak"))
	requireTableAbsent(t, filepath.Join(home, "control.db"), "repos")
	requireTableAbsent(t, filepath.Join(home, "control.db"), "repo_origins")
	requireTablePresent(t, filepath.Join(home, "control.db"), "repo_settings")
	requireConnectionColumnsPresent(t, filepath.Join(home, "repos", "alpha.db"))

	require.Contains(t, buf.String(), "alpha")
	require.Contains(t, buf.String(), "nothing was written")
}

// A lens naming a repo that no longer exists aborts, and says which.
func TestMigrateRegistry_DanglingLensRefAborts(t *testing.T) {
	home := buildLegacyHome(t)
	// Point the lens at a repo that was never there.
	execControl(t, filepath.Join(home, "control.db"),
		`UPDATE lenses SET write_repo = 'ghost'`)

	err := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "ghost")
	require.Contains(t, err.Error(), "--drop-dangling-lens-refs")
	require.NoFileExists(t, filepath.Join(home, "control.db.bak"), "the abort wrote nothing")
	require.FileExists(t, filepath.Join(home, "repos", "alpha.db"))

	// The escape hatch drops the lens and lets the rest through.
	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{DropDanglingLensRefs: true})))
	lensReg, err := repos.OpenLensRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer lensReg.Close()
	all, err := lensReg.List()
	require.NoError(t, err)
	require.Empty(t, all, "the lens whose write repo dangled was dropped")

	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1, "the repos still migrated")
}

// An already-migrated home is refused outright: with the connection columns
// already dropped there is nothing left to capture, and re-running would
// replace real repo_origins rows with nothing. --force does not override this.
func TestMigrateRegistry_RefusesAnAlreadyMigratedHome(t *testing.T) {
	home := buildLegacyHome(t)
	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{})))

	err := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// --force gets past the row count and straight into the next guard: the
	// database files are already named after registered uids.
	ferr := runMigrateRegistry(home, quietOpts(migrateOpts{Force: true}))
	require.Error(t, ferr)
	require.Contains(t, ferr.Error(), "named after a uid already in the repos table")

	// The origin the first run captured is untouched.
	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1)
	origins, err := repos.OpenOrigins(reg.DB(), testCryptFor(t, home))
	require.NoError(t, err)
	org, err := origins.Get(active[0].UID)
	require.NoError(t, err)
	require.NotNil(t, org)
	require.Equal(t, testAuthToken, org.AuthToken)
}

// Rolling control.db back to the pre-migration backup after a completed run
// leaves the registry empty while the repo databases have already lost their
// connection columns. There is nothing left to capture, so refuse rather than
// register repos with no origin. --force does not override this either.
func TestMigrateRegistry_RefusesRepoDatabasesThatOutranControlDB(t *testing.T) {
	home := buildLegacyHome(t)
	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{})))

	// The operator "undoes" the migration by restoring the backup — but the
	// repo databases are at schema 000017 and cannot be undone with it.
	bak, err := os.ReadFile(filepath.Join(home, "control.db.bak"))
	require.NoError(t, err)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(home, "control.db") + suffix)
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "control.db"), bak, 0o600))

	for _, opts := range []migrateOpts{{}, {Force: true}} {
		err := runMigrateRegistry(home, quietOpts(opts))
		require.Error(t, err)
		require.Contains(t, err.Error(), "migrated past schema 000017")
	}
}

// A run that committed control.db and then died before renaming any file is
// the one case --force exists for. The re-run must REUSE the uids the first
// attempt minted, or the lens rows it already wrote would dangle.
func TestMigrateRegistry_ForceResumesAfterAnInterruptedRun(t *testing.T) {
	home := buildLegacyHome(t)

	// Simulate the crash: plan and commit control.db, then stop — no renames,
	// no repo-database migration.
	plan, err := planMigration(home, migrateOpts{})
	require.NoError(t, err)
	firstUIDs := map[string]string{}
	for _, rp := range plan.Repos {
		firstUIDs[rp.Name] = rp.UID
	}
	require.NoError(t, applyControlDB(plan))
	require.FileExists(t, filepath.Join(home, "repos", "alpha.db"), "the crash was before the renames")

	// Without --force the re-run refuses, because the registry has rows.
	rerr := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, rerr)
	require.Contains(t, rerr.Error(), "--force")

	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{Force: true})))

	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, firstUIDs["alpha"], active[0].UID, "the re-run reuses the uid the lens rows point at")
	require.FileExists(t, filepath.Join(home, "repos", active[0].UID+".db"))

	lensReg, err := repos.OpenLensRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer lensReg.Close()
	lens, ok, err := lensReg.Get("workspace")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, active[0].UID, lens.WriteUID, "lens membership still resolves")
}

func TestMigrateRegistryCmd_FlagsRegistered(t *testing.T) {
	c := migrateRegistryCmd()
	for _, name := range []string{"home", "dry-run", "force", "drop-dangling-lens-refs"} {
		require.NotNil(t, c.Flags().Lookup(name), "missing --%s flag", name)
	}
}

// ---------------------------------------------------------------------------
// assertion helpers
// ---------------------------------------------------------------------------

type writerRecorder struct{ b []byte }

func (w *writerRecorder) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}
func (w *writerRecorder) String() string { return string(w.b) }

func execControl(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(stmt)
	require.NoError(t, err)
}

func requireTableAbsent(t *testing.T, path, table string) {
	t.Helper()
	db, err := openRaw(path)
	require.NoError(t, err)
	defer db.Close()
	ok, err := rawTableExists(db, table)
	require.NoError(t, err)
	require.False(t, ok, "table %q must not exist in %s", table, path)
}

func requireTablePresent(t *testing.T, path, table string) {
	t.Helper()
	db, err := openRaw(path)
	require.NoError(t, err)
	defer db.Close()
	ok, err := rawTableExists(db, table)
	require.NoError(t, err)
	require.True(t, ok, "table %q must exist in %s", table, path)
}

// requireLensSchemaIsUIDKeyed proves the lens tables were REBUILT rather than
// left alone: the legacy columns must be gone, not merely unused.
func requireLensSchemaIsUIDKeyed(t *testing.T, path string) {
	t.Helper()
	db, err := openRaw(path)
	require.NoError(t, err)
	defer db.Close()
	for _, c := range []struct{ table, column string }{
		{"lenses", "write_uid"},
		{"lens_reads", "repo_uid"},
	} {
		ok, cerr := rawColumnExists(db, c.table, c.column)
		require.NoError(t, cerr)
		require.True(t, ok, "%s.%s must exist", c.table, c.column)
	}
	for _, c := range []struct{ table, column string }{
		{"lenses", "write_repo"},
		{"lens_reads", "repo"},
	} {
		ok, cerr := rawColumnExists(db, c.table, c.column)
		require.NoError(t, cerr)
		require.False(t, ok, "legacy column %s.%s must be gone", c.table, c.column)
	}
}

func requireConnectionColumnsDropped(t *testing.T, path string) {
	t.Helper()
	db, err := openRaw(path)
	require.NoError(t, err)
	defer db.Close()
	for _, col := range []string{"url", "branch", "auth_method", "auth_token"} {
		ok, cerr := rawColumnExists(db, "remotes", col)
		require.NoError(t, cerr)
		require.False(t, ok, "remotes.%s must be dropped in %s", col, path)
	}
}

func requireConnectionColumnsPresent(t *testing.T, path string) {
	t.Helper()
	db, err := openRaw(path)
	require.NoError(t, err)
	defer db.Close()
	for _, col := range []string{"url", "branch", "auth_method", "auth_token"} {
		ok, cerr := rawColumnExists(db, "remotes", col)
		require.NoError(t, cerr)
		require.True(t, ok, "remotes.%s must still be there in %s", col, path)
	}
}
