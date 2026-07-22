package repos

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

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
