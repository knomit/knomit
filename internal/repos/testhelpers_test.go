package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// testRepoName is the repo the helpers below create. knomit has no default
// repo — Start opens only what the registry lists — so every test that needs
// one creates it explicitly and refers to it by this name. The literal is
// "core" purely so the on-disk paths in the adoption/migration tests keep
// naming the file an existing install would actually have.
const testRepoName = "core"

// mustCreateRepo creates name in m through the production Create path (preset
// mode, default ontology — the same seed the removed default-repo bootstrap
// wrote) and returns the live instance.
func mustCreateRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
	return ri
}

// newTestManager builds an unstarted Manager rooted at home, matching the Deps
// shape the lifecycle tests use (DisableBackgroundSync so opens are synchronous
// and deterministic). Each mutator may adjust Deps before construction — an
// origin root, an embedder, and so on. The caller drives Start itself:
// these tests are ABOUT Start's outcome, so it cannot be hidden in the helper.
func newTestManager(t *testing.T, home string, mutators ...func(*Deps)) *Manager {
	t.Helper()
	deps := Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	}
	for _, mutate := range mutators {
		mutate(&deps)
	}
	m := New(context.Background(), deps)
	t.Cleanup(func() { _ = m.Close() })
	return m
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
