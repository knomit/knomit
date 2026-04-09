package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// TestRepoInstance_Verify asserts that the wrapper delegates to Service.Verify
// under the read lock, stamps the report with the repo name, and returns a
// clean report for a fresh repo.
func TestRepoInstance_Verify(t *testing.T) {
	t.Log("Scenario: boot a fresh manager, call ri.Verify, expect clean report with Repo name stamped")
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Boot())
	ri := m.Get("knomit")
	require.NotNil(t, ri)

	report, err := ri.Verify(context.Background(), store.VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "fresh repo Verify must be clean: %v", report.Issues)
	require.Equal(t, "knomit", report.Repo)
}
