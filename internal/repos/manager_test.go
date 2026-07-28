package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestBoot_firstRunInitializesDefaultRepo verifies that Boot() succeeds on
// first run when the default core.db has no git data yet. This is a
// regression test for the isDefault=false bug where initDefaultGit() was
// never reachable, causing Boot() to fail on a fresh install.
func TestBoot_firstRunInitializesDefaultRepo(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
	})
	err := m.Start()
	require.NoError(t, err)
	require.NotNil(t, m.Get(config.DefaultRepoName), "default repo must be registered after Boot")
}

func TestStartRefusesUnrecoverableRepoWhenStrict(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reg, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Registered, but no DB file on disk and no origin to clone from.
	if err := reg.Upsert(RepoRecord{Name: "ghost", State: RepoActive}); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	m := newTestManager(t, home, func(d *Deps) { d.StrictMissing = true })
	err = m.Start()
	if err == nil {
		t.Fatal("Start succeeded; want refusal for an unrecoverable repo")
	}
	if !errors.Is(err, ErrRepoUnrecoverable) {
		t.Errorf("err = %v, want ErrRepoUnrecoverable", err)
	}
}

func TestStartToleratesUnrecoverableRepoWhenNotStrict(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(RepoRecord{Name: "ghost", State: RepoActive}); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	m := newTestManager(t, home, func(d *Deps) { d.StrictMissing = false })
	if err := m.Start(); err != nil {
		t.Fatalf("Start = %v, want nil (non-strict tolerates a missing repo)", err)
	}
}

// TestStartRebuildsMissingRepoFromOrigin is the payoff of registry-driven
// startup: a repo whose .db is gone but whose origin the registry remembers is
// re-cloned at boot instead of silently disappearing. This is the restored-
// from-backup case in miniature — the registry knows the repo should exist, the
// disk does not.
func TestStartRebuildsMissingRepoFromOrigin(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	withRoot := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, withRoot)
	require.NoError(t, first.Start())
	_, err := first.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	// Lose the database file, keeping control.db — exactly what a machine
	// looks like when the registry survived but the repo data did not.
	dbPath := filepath.Join(home, "repos", "cloned.db")
	require.NoError(t, os.Remove(dbPath))
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")

	second := newTestManager(t, home, withRoot)
	require.NoError(t, second.Start())
	require.NotNil(t, second.Get("cloned"), "repo must be rebuilt from its recorded origin")
	require.FileExists(t, dbPath)

	// StrictMissing must be satisfied by the rebuild, not tripped by it: the
	// repo was recoverable, so a strict boot has nothing to refuse.
	require.NoError(t, second.Close())
	require.NoError(t, os.Remove(dbPath))
	strict := newTestManager(t, home, withRoot, func(d *Deps) { d.StrictMissing = true })
	require.NoError(t, strict.Start())
	require.NotNil(t, strict.Get("cloned"))
}

// TestShutdown_concurrentSyncCancelUpdate verifies that Shutdown() does not
// race with concurrent writes to ri.syncCancel (as happens when ActivateSync
// is called while a shutdown is in progress). Run with -race to detect
// violations.
func TestShutdown_concurrentSyncCancelUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New(ctx, Deps{})
	ri := &RepoInstance{
		name:       "test",
		syncWg:     &sync.WaitGroup{},
		syncCancel: func() {},
	}
	m.Set("test", ri)

	const n = 500
	var wg sync.WaitGroup
	ready := make(chan struct{})

	// Simulate the write that startSync performs after restarting sync loops.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ready
		for i := 0; i < n; i++ {
			f := func() {}
			ri.mu.Lock()
			ri.syncCancel = f
			ri.mu.Unlock()
		}
	}()

	// Concurrently call Shutdown, which reads ri.syncCancel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ready
		for i := 0; i < n; i++ {
			m.Close()
		}
	}()

	close(ready)
	wg.Wait()
}
