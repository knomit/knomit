package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
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
	return bootNamedRepo(t, m, testRepoName)
}

// bootNamedRepo is bootRepo generalised to a caller-chosen name, for tests
// that need more than one repo (or a name other than testRepoName) in the
// same manager.
func bootNamedRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	require.NoError(t, m.Start())
	return createRepo(t, m, name)
}

// newTestManager returns an unstarted Manager rooted at a temp home whose
// machines are Synchronous, so no sync loop races the assertions. The index
// job still runs in the background: a test that needs it done waits with
// waitIndexSettled (Create already does).
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath: filepath.Join(home, "agent.key"),
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff},
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

// restoreLoggerLast registers the restore of the global logger. t.Cleanup is
// LIFO: call this BEFORE building the fixture (Manager, fleet), so the restore
// runs after the fixture's teardown and no goroutine of the fixture logs into
// the swapped logger while it is put back.
func restoreLoggerLast(t *testing.T) {
	t.Helper()
	prev := log.Logger
	t.Cleanup(func() { log.Logger = prev })
}

// writeAsPeer commits path on branch signed by ANOTHER key (the "peer"), then
// puts this instance's signer back. Store.SetSigner is a configuration call
// for a store that is not yet serving (every production caller runs it right
// after Open, before the first read or write). The signer field is atomic, so
// a swap cannot tear, but the trigger dispatcher (kicked by agent-branch
// advances only) signs with whichever key it loads: settle it before the peer
// key goes in so a run kicked by an earlier write cannot sign as the peer, and
// after the peer commit before restoring. A commit on another branch kicks the
// consensus merger instead (it may sign merge commits when consensus is on);
// settle does not cover it, and these tests do not enable consensus.
func writeAsPeer(t *testing.T, ri *RepoInstance, svc *store.Service, branch, path string) string {
	t.Helper()
	settle(t, ri)
	svc.SetSigner(testsigner.Named("peer"))
	h := writeOn(t, ri, branch, path)
	settle(t, ri)
	svc.SetSigner(nil) // back to the test fallback, this instance's key
	return h
}
