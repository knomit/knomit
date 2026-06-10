package repos

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestBoot_firstRunInitializesDefaultRepo verifies that Boot() succeeds on
// first run when the default trunk.db has no git data yet. This is a
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
