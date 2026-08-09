package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// macOS duplication artifacts: zero-byte files with a .db suffix. The real
	// target home has four of them. They open fine as empty SQLite databases
	// and must be skipped, not mistaken for already-migrated repos.
	for _, junk := range []string{"alpha 3.db", "core 1.db"} {
		require.NoError(t, os.WriteFile(filepath.Join(reposDir, junk), nil, 0o600))
	}

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

// A dry run reports the plan and changes nothing — not even the backup, and
// not the -wal/-shm companions a crashed server left behind.
func TestMigrateRegistry_DryRunWritesNothing(t *testing.T) {
	home := buildLegacyHome(t)
	// A database with an uncheckpointed WAL, as a crashed server leaves it.
	// Reading it through a read-WRITE handle would checkpoint the -wal into the
	// main file and unlink -wal/-shm on close: content-preserving, but not
	// "nothing". openRaw's mode=ro is what stops that, and the whole-tree
	// snapshot below is what keeps that honest.
	leaveStaleWAL(t, filepath.Join(home, "repos", "alpha.db"))
	require.FileExists(t, filepath.Join(home, "repos", "alpha.db-wal"))
	before := snapshotTree(t, home)

	var buf writerRecorder
	require.NoError(t, runMigrateRegistry(home, migrateOpts{DryRun: true, Out: &buf}))
	require.Equal(t, before, snapshotTree(t, home), "a dry run must leave every file exactly as it found it")

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
	execSQLite(t, filepath.Join(home, "control.db"),
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

// A zero-byte .db in repos/ — the macOS " 3.db" duplication artifact the target
// home is full of — is NOT a repo database that has already been migrated, and
// must not be reported as one. It is skipped, named, and left where it is.
func TestMigrateRegistry_SkipsFilesThatAreNotRepoDatabases(t *testing.T) {
	home := buildLegacyHome(t)
	junk := filepath.Join(home, "repos", "core 1.db")
	require.FileExists(t, junk, "the fixture seeds the artifact this test is about")

	var buf writerRecorder
	require.NoError(t, runMigrateRegistry(home, migrateOpts{Out: &buf}))

	// Named in the output, and NOT described as already migrated.
	require.Contains(t, buf.String(), "core 1.db")
	require.Contains(t, buf.String(), "not knomit repo databases")
	require.NotContains(t, buf.String(), "already been migrated")

	// Left exactly where it was, and never registered.
	require.FileExists(t, junk)
	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1, "only the real repo is registered")
	require.Equal(t, "alpha", active[0].Name)
}

// A file that is not SQLite at all might be somebody's data. Refuse rather than
// skip past it.
func TestMigrateRegistry_RefusesAnUnreadableDatabaseFile(t *testing.T) {
	home := buildLegacyHome(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "repos", "garbled.db"), []byte("this is not a database"), 0o600))

	err := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "garbled.db")
	require.NoFileExists(t, filepath.Join(home, "control.db.bak"), "the abort wrote nothing")
}

// One repo with an unresolvable HEAD must not block converting the others. It
// is registered with no repo_id, skipped by the duplicate survey, and reported.
func TestMigrateRegistry_DegradesOnAnUnresolvableHead(t *testing.T) {
	home := buildLegacyHome(t)
	// A real repo database whose refs are gone: HEAD no longer resolves.
	makeLegacyRepoDB(t, filepath.Join(home, "repos", "broken.db"),
		map[string]string{"kb/broken.md": "broken"},
		legacyRemote{url: "https://legacy.test/broken.git", branch: "main"})
	execSQLite(t, filepath.Join(home, "repos", "broken.db"), `DELETE FROM refs`)

	var buf writerRecorder
	require.NoError(t, runMigrateRegistry(home, migrateOpts{Out: &buf}))
	require.Contains(t, buf.String(), "HEAD unresolvable")
	require.Contains(t, buf.String(), "broken")

	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 2, "the healthy repo still migrated")

	broken, ok, err := reg.ByName("broken")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, broken.RepoID, "registered with no repo_id, to be filled in on first open")
	require.Equal(t, repos.ProfileCode, broken.Profile, "no profile: repo_settings is keyed by root commit")

	// Its origin still came across — the capture never depended on the walk.
	origins, err := repos.OpenOrigins(reg.DB(), testCryptFor(t, home))
	require.NoError(t, err)
	org, err := origins.Get(broken.UID)
	require.NoError(t, err)
	require.NotNil(t, org)
	require.Equal(t, "https://legacy.test/broken.git", org.URL)

	// A NULL repo_id does not collide with another NULL one: the uniqueness
	// index is partial, so two unresolvable repos both register.
	alpha, ok, err := reg.ByName("alpha")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, alpha.RepoID)
}

// Renaming databases out from under a live server is the one way this tool can
// corrupt data, so a running.marker stops it before the plan is even computed.
func TestMigrateRegistry_RefusesWhileTheServerMarkerIsPresent(t *testing.T) {
	home := buildLegacyHome(t)
	marker := filepath.Join(home, "running.marker")
	require.NoError(t, os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644))

	err := runMigrateRegistry(home, quietOpts(migrateOpts{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "running.marker")
	require.Contains(t, err.Error(), "--ignore-running-marker")
	require.FileExists(t, filepath.Join(home, "repos", "alpha.db"), "nothing ran")
	require.NoFileExists(t, filepath.Join(home, "control.db.bak"))

	// --dry-run and --force do NOT override it; only the dedicated flag does.
	require.Error(t, runMigrateRegistry(home, quietOpts(migrateOpts{DryRun: true})))
	require.Error(t, runMigrateRegistry(home, quietOpts(migrateOpts{Force: true})))

	require.NoError(t, runMigrateRegistry(home, quietOpts(migrateOpts{IgnoreRunningMarker: true})))
	require.NoFileExists(t, filepath.Join(home, "repos", "alpha.db"))
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

// The entire ordering invariant rests on this: a legacy repo database can be
// read with the stock sqlite3 driver in a process where sqlite-vec has NEVER
// been registered, which is exactly the state the tool is in during its
// capture and survey phases. Every other test in this file builds its fixture
// through store.Open, whose registerVec calls sqlite_vec.Auto() — a
// PROCESS-GLOBAL auto-extension — so in-suite raw handles do have vec and
// cannot detect a regression here.
//
// So the check runs in a SUBPROCESS that executes nothing but the helper
// below. The binary still LINKS sqlite-vec (it is the same test binary), which
// makes this the exact situation the tool runs in: linked, not registered.
func TestRawReadWithoutVecExtension(t *testing.T) {
	home := buildLegacyHome(t)
	dbPath := filepath.Join(home, "repos", "alpha.db")

	cmd := exec.Command(os.Args[0], "-test.run=^TestRawReadWithoutVecExtension_Subprocess$", "-test.v")
	cmd.Env = append(os.Environ(), vecLessDBEnv+"="+dbPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "raw reads must work without sqlite-vec registered:\n%s", out)
	require.Contains(t, string(out), "PASS")
}

// vecLessDBEnv carries the database path into the subprocess and is what makes
// the helper below a no-op during a normal test run.
const vecLessDBEnv = "KNOMIT_TEST_VECLESS_DB"

// TestRawReadWithoutVecExtension_Subprocess is the helper half of the test
// above. It runs ONLY when re-executed with vecLessDBEnv set, and it must never
// call store.Open — doing so would register sqlite-vec and make the assertion
// vacuous.
func TestRawReadWithoutVecExtension_Subprocess(t *testing.T) {
	dbPath := os.Getenv(vecLessDBEnv)
	if dbPath == "" {
		t.Skip("helper process; runs only when re-executed by TestRawReadWithoutVecExtension")
	}

	// Guard against the test quietly becoming vacuous: store.Open registers the
	// "sqlite3_knomit" driver at the same moment it calls sqlite_vec.Auto(), so
	// the driver's ABSENCE is proof the auto-extension was never installed.
	for _, name := range sql.Drivers() {
		require.NotEqual(t, "sqlite3_knomit", name,
			"sqlite-vec has been registered in this process; the test proves nothing")
	}

	db, err := openRaw(dbPath)
	require.NoError(t, err)
	defer db.Close()

	// The database DOES contain a persisted vec0 virtual table; reading the
	// plain tables around it must still work.
	ok, err := rawTableExists(db, "facts_vec")
	require.NoError(t, err)
	require.True(t, ok, "the fixture must actually contain a vec0 table for this to mean anything")

	origin, err := readLegacyOrigin(db)
	require.NoError(t, err)
	require.NotNil(t, origin)
	require.Equal(t, "https://legacy.test/alpha.git", origin.URL)

	root, born, err := readRootCommit(db)
	require.NoError(t, err)
	require.Len(t, root, 40)
	require.NotZero(t, born)
}

func TestMigrateRegistryCmd_FlagsRegistered(t *testing.T) {
	c := migrateRegistryCmd()
	for _, name := range []string{
		"home", "dry-run", "force", "drop-dangling-lens-refs", "ignore-running-marker",
	} {
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

// snapshotTree fingerprints every DATA-BEARING file under dir by path, size and
// content hash. Used to assert that a phase left the home untouched.
//
// Two exclusions, both measured rather than assumed. Even a mode=ro open
// touches SQLite's shared-memory bookkeeping: the -shm index header is
// rewritten, and an EMPTY -wal is created for a WAL database that had none.
// Neither carries data — an empty -wal is by definition zero committed frames,
// and -shm is a rebuildable index over the -wal — and both are cleaned up by
// the next clean read-write close.
//
// A NON-empty -wal is very much data-bearing and is compared: it is the
// difference between "we read the database" and "we checkpointed it", which is
// exactly what this snapshot exists to catch.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, "-shm") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if len(data) == 0 && strings.HasSuffix(path, "-wal") {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out[rel] = fmt.Sprintf("%d:%x", len(data), sha256.Sum256(data))
		return nil
	}))
	return out
}

// leaveStaleWAL puts path into the state a crashed server leaves: a committed
// transaction still sitting in an uncheckpointed -wal. Achieved by copying the
// three files aside while a writer still holds them and restoring them after
// the writer closes (a clean close would checkpoint and unlink them).
func leaveStaleWAL(t *testing.T, path string) {
	t.Helper()
	live, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	require.NoError(t, err)
	live.SetMaxOpenConns(1)
	_, err = live.Exec(`CREATE TABLE zz_stale (x); INSERT INTO zz_stale VALUES (1)`)
	require.NoError(t, err)

	stash := t.TempDir()
	suffixes := []string{"", "-wal", "-shm"}
	for _, sfx := range suffixes {
		data, rerr := os.ReadFile(path + sfx)
		if rerr != nil {
			continue
		}
		require.NoError(t, os.WriteFile(filepath.Join(stash, "c"+sfx), data, 0o600))
	}
	require.NoError(t, live.Close())
	for _, sfx := range suffixes {
		data, rerr := os.ReadFile(filepath.Join(stash, "c"+sfx))
		if rerr != nil {
			continue
		}
		require.NoError(t, os.WriteFile(path+sfx, data, 0o600))
	}
}

func execSQLite(t *testing.T, path, stmt string) {
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
