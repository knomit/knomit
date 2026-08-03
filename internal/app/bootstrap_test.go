package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/backup"
	"knomit/internal/config"
	"knomit/internal/repos"
)

func baseCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	return cfg
}

// backupCfg returns a config whose replica is a local file:// bucket — the same
// litestream code path as S3, with no network.
func backupCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := baseCfg(t)
	cfg.AgentName = "prod-1"
	cfg.Backup = config.BackupConfig{
		Enabled: true,
		URL:     "file://" + t.TempDir(),
		// The agent binary is built by TestMain rather than located: under
		// `go test` there is no knomit executable to sit beside, so the search
		// would find nothing and Open would fail — leaving these tests exercising
		// a boot with no replication at all rather than the paths they name.
		AgentPath:         backupAgentBin,
		Instance:          "prod-1",
		SnapshotInterval:  time.Hour,
		SnapshotRetention: time.Hour,
		L0Retention:       time.Minute,
		MonitorInterval:   50 * time.Millisecond,
	}
	return cfg
}

// injectKey writes the SSH key a backup-enabled instance is required to be
// given. Production mounts it as a secret; the test generates it up front,
// which is the same thing from Bootstrap's point of view: the key EXISTS
// before boot, so the agent branch is stable across restores.
func injectKey(t *testing.T, cfg config.Config) {
	t.Helper()
	if _, _, err := ensureKeyPair(keyPathFor(cfg)); err != nil {
		t.Fatalf("inject key: %v", err)
	}
}

func TestBootstrapRequiresAgentNameWhenBackupEnabled(t *testing.T) {
	cfg := backupCfg(t)
	cfg.AgentName = "" // missing
	injectKey(t, cfg)

	_, err := Bootstrap(context.Background(), cfg)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired", err)
	}
}

func TestBootstrapRequiresInjectedKeyWhenBackupEnabled(t *testing.T) {
	cfg := backupCfg(t)
	// No key file exists at cfg.Home/id_ed25519, so generating one would be the
	// silent-fork bug: a fresh fingerprint means a fresh branch.

	_, err := Bootstrap(context.Background(), cfg)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired (key must be injected, not generated)", err)
	}
	if _, err := os.Stat(keyPathFor(cfg)); !os.IsNotExist(err) {
		t.Error("a key was generated anyway; a backup-enabled instance must never mint its own identity")
	}
}

func TestBootstrapGeneratesKeyWhenBackupDisabled(t *testing.T) {
	cfg := baseCfg(t)
	cfg.Backup.Enabled = false

	res, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil (non-backup instances keep generating keys)", err)
	}
	if res.Signer == nil {
		t.Error("Signer is nil")
	}
	if res.AgentBranch == "" {
		t.Error("AgentBranch is empty")
	}
	if res.Backup != nil {
		t.Error("Backup manager built while disabled")
	}
}

func TestBootstrapAgentBranchIsStableAcrossRuns(t *testing.T) {
	cfg := baseCfg(t)
	cfg.AgentName = "prod-1"

	first, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	// Second run over the same home reuses the persisted key.
	second, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if first.AgentBranch != second.AgentBranch {
		t.Errorf("agent branch changed across runs: %q then %q", first.AgentBranch, second.AgentBranch)
	}
	if _, err := os.Stat(filepath.Join(cfg.Home, "id_ed25519")); err != nil {
		t.Errorf("key not persisted: %v", err)
	}
}

// TestBootstrapRestoresControlThenRepos is the ordering test. It replicates a
// control.db plus one repo database, wipes the machine, and boots: control.db
// has to come back FIRST, because the registry inside it is the only record of
// which repo databases are intended — and every restore has to land before
// anything opens a database, since restore refuses to overwrite a file that
// exists.
func TestBootstrapRestoresControlThenRepos(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)
	controlPath := filepath.Join(cfg.Home, "control.db")
	repoPath := filepath.Join(cfg.Home, "repos", "core.db")

	seedRegistry(t, controlPath, "core")
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seedDB(t, repoPath, "hello")

	// Replicate both, then take the machine away.
	m, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	if err := m.Track("control", controlPath); err != nil {
		t.Fatalf("Track control: %v", err)
	}
	if err := m.Track("core", repoPath); err != nil {
		t.Fatalf("Track core: %v", err)
	}
	waitSynced(t, m, "control", "core")
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wipeDB(t, controlPath)
	wipeDB(t, repoPath)

	boot, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil", err)
	}
	t.Cleanup(func() { boot.Backup.Close(context.Background()) })

	if _, err := os.Stat(controlPath); err != nil {
		t.Fatalf("control.db not restored: %v", err)
	}
	if _, err := os.Stat(repoPath); err != nil {
		t.Fatalf("repos/core.db not restored — the registry inside control.db is what names it: %v", err)
	}
	assertRow(t, repoPath, "hello")
}

// TestBootstrapStartsWhenARestoreFails pins the rule that replaced the old
// refuse-the-boot behaviour: the replica is a warm-start CACHE, so a restore
// that fails costs boot TIME and nothing else. The repo is re-cloned from the
// origin its registry row records, and turning that into a refusal would convert
// a cache miss into an outage.
//
// The fixture breaks the RESTORE and nothing else: a repo whose .db is therefore
// simply ABSENT, with a real snapshot waiting in the replica so the restore
// genuinely tries and genuinely fails. A read-only repos/ directory reproduces
// exactly that.
func TestBootstrapStartsWhenARestoreFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny writes")
	}
	cfg := backupCfg(t)
	injectKey(t, cfg)
	controlPath := filepath.Join(cfg.Home, "control.db")
	reposDir := filepath.Join(cfg.Home, "repos")
	repoPath := filepath.Join(reposDir, "core.db")

	seedRegistry(t, controlPath, "core")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDB(t, repoPath, "hello")

	m, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	if err := m.Track("control", controlPath); err != nil {
		t.Fatalf("Track control: %v", err)
	}
	if err := m.Track("core", repoPath); err != nil {
		t.Fatalf("Track core: %v", err)
	}
	waitSynced(t, m, "control", "core")
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wipeDB(t, controlPath)
	wipeDB(t, repoPath)

	// The snapshot exists and restore will reach for it — but cannot write.
	if err := os.Chmod(reposDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(reposDir, 0o755) }) // so TempDir cleanup can run

	boot, err := Bootstrap(context.Background(), cfg)
	if boot != nil && boot.Backup != nil {
		boot.Backup.Close(context.Background())
	}
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil: a failed restore must NOT refuse the boot — the repo is "+
			"rebuilt from its origin, and refusing turns a cache miss into an outage", err)
	}
	if boot == nil {
		t.Fatal("Bootstrap returned no BootResult despite returning no error")
	}
	// The precondition that makes this test meaningful: the restore really did
	// fail, so the database really is absent and repos.Manager.Start is what has
	// to rebuild it.
	if _, serr := os.Stat(repoPath); !os.IsNotExist(serr) {
		t.Fatalf("fixture is wrong: %s exists, so this is not the failed-restore case", repoPath)
	}
}

// TestNewRejectsAHandBuiltBootResult: the ordering guarantee is enforced, not
// merely documented. A BootResult that did not come from Bootstrap carries a
// plausible signer and branch but means no restore ran.
// TestBootstrapWithholdsControlReplicationWhenItsRestoreFailed pins the single
// exception to "the replica is a cache, so nothing here can refuse anything".
//
// Every other database Bootstrap touches is rebuildable from git, so a failed
// restore costs boot time and the empty replacement is safe to replicate.
// control.db is not: its registry is the only record of which repos exist and
// which origin each came from, and that lives nowhere else. Opening the
// registry CREATES an empty control.db, so a boot whose restore failed is
// holding an empty file — and replicating it would let litestream re-anchor
// against the replica and snapshot the emptiness as the new head, destroying the
// registry the restore could not read. A permission error, a network blip or a
// budget overrun would each be enough.
//
// The boot still succeeds. It just declines to write over what it could not
// read, and says so.
func TestBootstrapWithholdsControlReplicationWhenItsRestoreFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny writes")
	}
	cfg := backupCfg(t)
	injectKey(t, cfg)
	controlPath := filepath.Join(cfg.Home, "control.db")

	// A real backup of a real registry, so the restore below has something to
	// reach for and "no snapshot" cannot be what happens instead.
	seedRegistry(t, controlPath, "core")
	m, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	if err := m.Track("control", controlPath); err != nil {
		t.Fatalf("Track control: %v", err)
	}
	waitSynced(t, m, "control")
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wipeDB(t, controlPath)

	// The snapshot exists and the restore will reach for it — but cannot write.
	if err := os.Chmod(cfg.Home, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.Home, 0o755) }) // so TempDir cleanup can run

	boot, err := Bootstrap(context.Background(), cfg)
	if boot != nil && boot.Backup != nil {
		boot.Backup.Close(context.Background())
	}
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil: a failed control restore must not refuse the boot either", err)
	}
	// The precondition that makes the assertion mean anything: the restore
	// really did fail, so control.db really is absent and the next thing to open
	// the registry would create an empty one.
	if _, serr := os.Stat(controlPath); !os.IsNotExist(serr) {
		t.Fatalf("fixture is wrong: %s exists, so this is not the failed-restore case", controlPath)
	}
	if boot.ReplicateControl {
		t.Error("ReplicateControl is true after the control.db restore FAILED. The empty registry this " +
			"boot is about to create would be snapshotted over the good one in the replica, and the repo " +
			"names and origin URLs inside it are reconstructible from nothing — not from git, not from a " +
			"re-clone. A transient permission or network error would become permanent data loss")
	}
}

// TestBootstrapReplicatesControlOnASuccessfulRestore is the other half of the
// guard above. Withholding replication is the safe direction, so a bug that
// withholds it ALWAYS would be invisible — and would silently stop backing up
// the registry on every healthy boot.
func TestBootstrapReplicatesControlOnASuccessfulRestore(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)
	controlPath := filepath.Join(cfg.Home, "control.db")

	seedRegistry(t, controlPath, "core")
	m, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	if err := m.Track("control", controlPath); err != nil {
		t.Fatalf("Track control: %v", err)
	}
	waitSynced(t, m, "control")
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wipeDB(t, controlPath)

	boot, err := Bootstrap(context.Background(), cfg)
	if boot != nil && boot.Backup != nil {
		defer boot.Backup.Close(context.Background())
	}
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !boot.ReplicateControl {
		t.Error("ReplicateControl is false after control.db restored cleanly; the registry would stop " +
			"being backed up on an ordinary healthy boot")
	}
}

// TestBootstrapReplicatesControlWhenThereIsNoSnapshot: an empty replica is a
// first boot, not a failure. Withholding replication here would mean a brand
// new instance never backs up its registry at all — the guard would defeat the
// feature on exactly the deployment that has the most to gain from it.
func TestBootstrapReplicatesControlWhenThereIsNoSnapshot(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)

	boot, err := Bootstrap(context.Background(), cfg)
	if boot != nil && boot.Backup != nil {
		defer boot.Backup.Close(context.Background())
	}
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !boot.ReplicateControl {
		t.Error("ReplicateControl is false on a first boot with an empty replica. " +
			"'No backup exists' is not 'the restore failed', and conflating them means a fresh " +
			"instance never starts replicating its registry")
	}
}

func TestNewRejectsAHandBuiltBootResult(t *testing.T) {
	cfg := baseCfg(t)
	a, err := New(context.Background(), cfg, &BootResult{AgentBranch: "agent/forged"}, Options{})
	if a != nil {
		a.Close()
	}
	if err == nil {
		t.Fatal("New accepted a BootResult that never went through Bootstrap")
	}
	if !strings.Contains(err.Error(), "Bootstrap") {
		t.Errorf("error %q should name Bootstrap as the fix", err)
	}
}

// TestBootstrapAcceptsNoSnapshot covers the first boot of a backup-enabled
// instance and the repo that needs an origin clone: the replica holds nothing
// for a registered repo. That is not a failure, and refusing it would make a
// backup-enabled instance impossible to start for the first time.
func TestBootstrapAcceptsNoSnapshot(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)
	seedRegistry(t, filepath.Join(cfg.Home, "control.db"), "core")

	boot, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil (an empty replica is how a first boot looks)", err)
	}
	t.Cleanup(func() { boot.Backup.Close(context.Background()) })
	if boot.Backup == nil {
		t.Fatal("Backup manager is nil while backup is enabled")
	}
}

// TestBootstrapZeroReposIsValid: knomit has no default repo, so an empty
// registry is an ordinary state and must boot.
func TestBootstrapZeroReposIsValid(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)

	boot, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap = %v, want nil (zero repos is a valid state)", err)
	}
	t.Cleanup(func() { boot.Backup.Close(context.Background()) })
}

// --- helpers ---

func seedRegistry(t *testing.T, controlPath string, names ...string) {
	t.Helper()
	reg, err := repos.OpenRepoRegistry(controlPath)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	for _, name := range names {
		if err := reg.Upsert(repos.RepoRecord{Name: name, State: repos.RepoActive}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}
}

func seedDB(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, value); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func assertRow(t *testing.T, path, want string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&got); err != nil {
		t.Fatalf("query %s: %v", path, err)
	}
	if got != want {
		t.Errorf("restored value = %q, want %q", got, want)
	}
}

// wipeDB removes a database, its sidecars, and its litestream shadow directory
// — the fresh volume a restore actually lands on.
func wipeDB(t *testing.T, path string) {
	t.Helper()
	dir, file := filepath.Split(path)
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "."+file+"-litestream")} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
	}
}

func waitSynced(t *testing.T, m *backup.Manager, names ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		synced := map[string]bool{}
		for _, st := range m.Status(context.Background()) {
			if st.InSync && st.RemoteTXID > 0 {
				synced[st.Name] = true
			}
		}
		all := true
		for _, name := range names {
			if !synced[name] {
				all = false
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %v to replicate", names)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestBootstrapIdentityNeverOpensTheReplica is the guard behind `knomit
// verify`, which must be a read-only check of what is on THIS volume.
//
// Full Bootstrap spawns the knomit-backup child, probes the bucket, and
// restores control.db and every absent repo database. All three are wrong for
// verify: the restore WRITES to KNOMIT_HOME, the agent is a second litestream
// process against the same replica prefix a running server is already using,
// and an unreachable bucket would delay a command that only ever needed the
// local files.
//
// The config here has backup fully enabled with a reachable file:// replica, so
// a regression that called Bootstrap would succeed and this test would still
// catch it — the assertion is on the ABSENCE of a replica client, not on an
// error.
func TestBootstrapIdentityNeverOpensTheReplica(t *testing.T) {
	cfg := backupCfg(t)
	injectKey(t, cfg)

	res, err := BootstrapIdentity(cfg)
	if err != nil {
		t.Fatalf("BootstrapIdentity: %v", err)
	}
	if res.Backup != nil {
		t.Error("BootstrapIdentity opened a replica client; verify would then hold a second agent " +
			"against the prefix a running server is replicating to")
	}
	if res.ReplicateControl {
		t.Error("ReplicateControl must stay false: nothing that goes through BootstrapIdentity replicates")
	}
	if res.Signer == nil || res.AgentBranch == "" {
		t.Fatalf("identity not resolved: signer=%v branch=%q", res.Signer, res.AgentBranch)
	}
	// The whole point of the exercise: app.New accepts it, so verify can boot.
	if !res.bootstrapped {
		t.Error("BootstrapIdentity must mark the result bootstrapped, or app.New refuses it")
	}
	// Nothing was fetched, so nothing was written.
	if _, err := os.Stat(filepath.Join(cfg.Home, "control.db")); !os.IsNotExist(err) {
		t.Errorf("control.db exists after BootstrapIdentity (stat err = %v); it must not restore anything", err)
	}
}

// TestBootstrapIdentityStillRequiresAStableIdentity pins that the relaxation is
// about the REPLICA and nothing else.
//
// The key check keys off backup.enabled rather than off whether this call opens
// the replica, because the hazard is not the replica: a generated key means a
// fresh agent branch, so anything that opens these repos and writes would write
// to a branch the restored history does not live on. That is as true of verify
// as it is of serve.
func TestBootstrapIdentityStillRequiresAStableIdentity(t *testing.T) {
	cfg := backupCfg(t) // backup enabled, but no key injected

	if _, err := BootstrapIdentity(cfg); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired", err)
	}

	cfg2 := backupCfg(t)
	cfg2.AgentName = ""
	injectKey(t, cfg2)
	if _, err := BootstrapIdentity(cfg2); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired for an empty agent_name", err)
	}
}
