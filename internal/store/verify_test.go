package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerify_FreshRepoIsClean asserts that a freshly initialised store with
// no facts written reports IsClean() == true and zero issues.
func TestVerify_FreshRepoIsClean(t *testing.T) {
	t.Log("Scenario: open a fresh store, init repo, run Verify, expect clean report")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "fresh repo must be clean, got issues: %v", report.Issues)
	require.True(t, report.IsStrictlyClean(), "fresh repo must be strictly clean")
	require.Contains(t, report.Branches, "agent/test")
}
