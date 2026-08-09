package repos

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestStart_freshHomeHasNoRepos pins the first-run contract: Start succeeds on
// an empty home and registers NOTHING. knomit has no default repo — not "core",
// not any other name — so a fresh install serves zero repos until the user
// creates one, and zero is a healthy state rather than a failure to boot.
func TestStart_freshHomeHasNoRepos(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start(), "an empty home must boot, not error")
	t.Cleanup(func() { _ = m.Close() })

	require.Empty(t, m.Names(), "a fresh home must register no repos")
	require.Nil(t, m.Get("core"), `"core" must not be conjured into existence`)

	// The empty manager is fully functional: a repo created now registers
	// normally, and a second Start over the same home re-opens it.
	createRepo(t, m, "work")
	require.Equal(t, []string{"work"}, m.Names())
}

// TestStart_reopensExistingReposOnly pins the other half: Start opens every
// registered repo and still creates none of its own.
func TestStart_reopensExistingReposOnly(t *testing.T) {
	dir := t.TempDir()
	boot := func() *Manager {
		m := New(context.Background(), Deps{
			Cfg:                   config.Config{Home: dir},
			AgentBranch:           "machine/test",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		return m
	}

	m1 := boot()
	createRepo(t, m1, "alpha")
	createRepo(t, m1, "beta")
	require.NoError(t, m1.Close())

	m2 := boot()
	t.Cleanup(func() { _ = m2.Close() })
	require.ElementsMatch(t, []string{"alpha", "beta"}, m2.Names(),
		"reboot must re-open exactly the repos on disk — no more, no fewer")
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
