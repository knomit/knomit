package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// testRepoName is the repo these tests create when they need one. It carries no
// meaning to the product — Manager.Start creates nothing and privileges no
// name — it is just the fixture every test in this package happens to share.
const testRepoName = "core"

// bootRepo starts m and creates testRepoName in it, returning the instance.
//
// Booting a manager registers only repos that already exist on disk, so a test
// that wants one must say so. Use this wherever a test previously assumed
// Start had produced a default repo.
func bootRepo(t *testing.T, m *Manager) *RepoInstance {
	t.Helper()
	require.NoError(t, m.Start())
	return createRepo(t, m, testRepoName)
}

// newTestManager returns an unstarted Manager rooted at a temp home, with
// background sync disabled so the loops cannot race assertions.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch:           "agent/test",
		KeyPath:               filepath.Join(home, "agent.key"),
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })
	return m
}

// createRepo creates a preset-ontology repo named name in an already-started
// manager, via the same Create path the REST API uses.
func createRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
	return ri
}

// reopenTestRepo re-opens testRepoName directly via Add, standing in for what
// Start will do automatically once Task 6 writes registry rows on Create.
// Many reboot tests in this package close a manager and boot a fresh one over
// the same home to exercise reopen behaviour (ensureBranch adoption, ontology
// refresh, index heal); until Create registers a repo, a fresh Manager's
// Start finds nothing, so those tests re-open the known db file by hand. home
// is the manager's Cfg.Home; the db file must already exist there from an
// earlier boot.
func reopenTestRepo(t *testing.T, m *Manager, home string) *RepoInstance {
	t.Helper()
	dbPath := filepath.Join(home, "repos", testRepoName+".db")
	require.NoError(t, m.Add(testRepoName, "", dbPath, nil))
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	return ri
}

// testService resolves the instance's current store via the production
// Acquire path, failing the test if no store is attached. The acquisition is
// released before returning: several tests close and reopen the manager
// mid-test, and an acquisition held until t.Cleanup would deadlock that close
// (it drains acquirers). Tests are single-goroutine, so the returned service
// stays valid until the test itself tears the repo down.
func testService(t *testing.T, ri *RepoInstance) *store.Service {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	release()
	return svc
}
