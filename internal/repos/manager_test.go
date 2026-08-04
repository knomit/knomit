package repos

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStart_FreshHomeComesUpWithZeroRepos pins the state knomit never used to
// have: a brand-new home, an empty registry, no .db files — and a server that
// starts anyway, serving no repos at all.
//
// It replaces the old first-run test, which asserted the exact opposite (that
// Start conjured a default repo named core). That auto-creation was the last
// path that could bring a repo into existence WITHOUT going through the
// registry: core.db absent meant a fresh, empty core, so a home whose repo data
// was lost came back up looking healthy and holding nothing.
// Nothing is created implicitly now: the user creates every repo explicitly,
// and every repo without exception arrives through the registry reconcile.
func TestStart_FreshHomeComesUpWithZeroRepos(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)

	require.NoError(t, m.Start(), "a fresh home with no repos must still boot")
	require.Empty(t, m.Names(), "a fresh home must come up with zero repos")

	// No stray database was written on the side, under any name.
	entries, err := os.ReadDir(filepath.Join(home, "repos"))
	require.NoError(t, err, "the repos dir is still created; it is just empty")
	require.Empty(t, entries, "boot must not create any repo database: %v", entries)

	// And the registry agrees there is nothing to open.
	reg := m.RepoRegistry()
	require.NotNil(t, reg)
	active, err := reg.List(RepoActive)
	require.NoError(t, err)
	require.Empty(t, active, "boot must not register a repo nothing asked for")

	// Zero repos is a working state, not a broken one: creating the first repo
	// from here is an ordinary Create.
	ri := mustCreateRepo(t, m, "first")
	require.Equal(t, "first", ri.Name())
	require.Equal(t, []string{"first"}, m.Names())
}

// TestStart_AdoptsExistingCoreDBAsOrdinaryRepo is the migration story for
// removing the default repo. An install that predates the registry has
// repos/core.db on disk and no control.db. On the first boot after this change
// the filesystem adoption registers that database as an ORDINARY repo: the
// user keeps their data and their repo, it simply stops being privileged.
//
// "Ordinary" is the load-bearing half. core used to be un-archivable by name,
// so a migration that left any special-casing behind would be invisible until a
// user tried to remove it.
func TestStart_AdoptsExistingCoreDBAsOrdinaryRepo(t *testing.T) {
	home := t.TempDir()

	// Build the legacy on-disk state: a repo named core with real content in it.
	const factPath = "kb/notes/pre-migration.md"
	const factBody = "---\ntype: observation\nconfidence: 0.5\nsources: 1\ndomain: [migration]\n" +
		"entities: []\nrefs: []\n---\n# fact\n\nwritten before the default repo was removed\n"
	seed := newTestManager(t, home)
	require.NoError(t, seed.Start())
	seedRI := mustCreateRepo(t, seed, "core")
	_, err := testService(t, seedRI).Facts().WriteFact(
		context.Background(), "machine/test", factPath, factBody,
		"test: pre-migration content", "created")
	require.NoError(t, err)
	require.NoError(t, seed.Close())

	// Drop control.db: an install from before the registry existed has the .db
	// files and nothing else. The filesystem is the only record of what exists.
	require.NoError(t, os.Remove(filepath.Join(home, "control.db")))
	require.FileExists(t, filepath.Join(home, "repos", "core.db"))

	// First boot after the change.
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	ri := m.Get("core")
	require.NotNil(t, ri, "an existing core.db must be adopted, not dropped")
	require.Equal(t, []string{"core"}, m.Names())

	// The data came with it.
	res, err := testService(t, ri).Facts().ReadFact(context.Background(), "machine/test", factPath, nil)
	require.NoError(t, err, "adopted repo must still hold its facts")
	require.Contains(t, res.Content, "written before the default repo was removed")

	// It is registered like any other repo, so the NEXT boot is a plain
	// registry reconcile rather than another filesystem scan.
	reg := m.RepoRegistry()
	require.NotNil(t, reg)
	active, err := reg.List(RepoActive)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "core", active[0].Name)
	require.Equal(t, "", active[0].ArchiveID, "an active row carries an empty archive_id")

	// And it carries no privilege: the adopted core is archivable like any other
	// repo, even as the last one standing.
	_, err = m.Archive("core")
	require.NoError(t, err, "an adopted core must be an ordinary repo")
	require.Empty(t, m.Names())
}

// TestStartSkipsUnrecoverableRepoRatherThanFailingTheBoot pins the rule that
// no single repo can fail the boot.
//
// A row with no database and no origin is the one genuinely unrecoverable case
// — a repo created without an origin keeps its only copy of its git history
// inside that .db. It is still logged and skipped rather than refused, because
// one unrecoverable repo must not take every other healthy repo on the instance
// offline with it. The row survives either way, so the state stays diagnosable.
func TestStartSkipsUnrecoverableRepoRatherThanFailingTheBoot(t *testing.T) {
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

	m := newTestManager(t, home)
	require.NoError(t, m.Start(), "one unrecoverable repo must not fail the boot")
	require.Nil(t, m.Get("ghost"), "an unrecoverable repo must not be served")

	// The row is left behind on purpose: it is the only remaining record that
	// this repo was ever meant to exist.
	reg2, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg2.Close()
	_, found, err := reg2.ActiveRecord("ghost")
	require.NoError(t, err)
	require.True(t, found, "the registry row must survive so the state stays diagnosable")
}

// TestStartRebuildsMissingRepoFromOrigin is the payoff of registry-driven
// startup: a repo whose .db is gone but whose origin the registry remembers is
// re-cloned at boot instead of silently disappearing. This is the replaced-
// volume case in miniature — the registry knows the repo should exist, the disk
// does not.
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

	// The rebuild is repeatable, not a one-shot: lose the database again and the
	// next boot clones it again from the same recorded origin.
	require.NoError(t, second.Close())
	require.NoError(t, os.Remove(dbPath))
	third := newTestManager(t, home, withRoot)
	require.NoError(t, third.Start())
	require.NotNil(t, third.Get("cloned"))
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
