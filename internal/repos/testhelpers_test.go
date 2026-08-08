package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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

// createRepo creates a preset-ontology repo named name in an already-started
// manager, via the same Create path the REST API uses.
func createRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: "preset"}, nil)
	require.NoError(t, err)
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
