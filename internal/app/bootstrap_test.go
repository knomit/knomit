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
		Enabled:           true,
		URL:               "file://" + t.TempDir(),
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

// TestPreflightTargetsExcludesRestored pins requirement (A) of the boot order.
//
// Preflight compares local and remote transaction IDs. A database restore JUST
// created has no local litestream shadow directory, so it reads local TXID 0
// against a replica holding real history — indistinguishable, at the file level,
// from a stale volume that lost its shadow directory. The one thing that DOES
// separate them is knowledge Bootstrap has and Preflight does not: this file did
// not exist a moment ago, so it cannot be a stale volume. Bootstrap therefore
// never preflights what it just restored.
func TestPreflightTargetsExcludesRestored(t *testing.T) {
	intended := []repos.RepoRecord{{Name: "core"}, {Name: "notes"}, {Name: "scratch"}}
	got := preflightTargets(intended, []string{"notes"})
	want := []string{"core", "scratch"}
	if len(got) != len(want) {
		t.Fatalf("preflightTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("preflightTargets = %v, want %v", got, want)
		}
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

// TestBootstrapRefusesWhenARestoreFails pins the refuse-the-boot rule. Starting
// degraded is destructive here rather than merely incomplete: replication is
// about to start, so empty local state would be replicated OVER the good backup.
//
// The fixture has to break the RESTORE and nothing else. An earlier version put
// a regular file where the repos directory belongs, which also broke Preflight
// (it opens the litestream state directory beneath repos/) — so the boot was
// refused either way and the check under test was never exercised. The shape
// that matters is the realistic one: a repo whose restore errors and whose .db
// is therefore simply ABSENT, which preflights to nil and would otherwise sail
// straight into app.New. A read-only repos/ directory reproduces exactly that,
// with a real snapshot waiting in the replica so the restore genuinely tries.
func TestBootstrapRefusesWhenARestoreFails(t *testing.T) {
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
	if !errors.Is(err, ErrRestoreIncomplete) {
		t.Fatalf("Bootstrap = %v, want ErrRestoreIncomplete: a failed restore must refuse the boot, not start empty", err)
	}
	// The precondition that makes this test meaningful: with the .db absent,
	// Preflight passes. Nothing but the Failed check stands between this state
	// and app.New.
	if _, serr := os.Stat(repoPath); !os.IsNotExist(serr) {
		t.Fatalf("fixture is wrong: %s exists, so this is not the absent-database case", repoPath)
	}
}

// TestNewRejectsAHandBuiltBootResult: the ordering guarantee is enforced, not
// merely documented. A BootResult that did not come from Bootstrap carries a
// plausible signer and branch but means no restore ran.
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
