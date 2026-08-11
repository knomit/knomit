package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// newRenameTestRepo stands up a started Manager with one registered repo
// ("alpha") so the test can drive Manager.RenameRepo — the production rename
// path (internal/repos/lifecycle.go) — rather than faking a rename by hand.
// Mirrors newLensE2E's single-repo half; a lens is unnecessary here since the
// property under test (cursor survives a repo rename) only needs a
// lens-of-one binding.
func newRenameTestRepo(t *testing.T) (m *repos.Manager, ri *repos.RepoInstance, ctx context.Context) {
	t.Helper()

	dir := t.TempDir()
	m = repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri = newE2ERepo(t, m, "alpha")
	m.Set("alpha", ri)

	return m, ri, repos.WithRepoInstance(context.Background(), ri)
}

// TestQueryResume_SurvivesRepoRename pins the fix this task exists for: before
// the cursor pin moved onto ids (repo:<uid> / lens:<uid>), the resume check
// compared Binding.Name() against the stored name, so a repo rename between
// mint and resume changed Name() and silently orphaned every in-flight
// cursor. Renaming the repo mid-walk must no longer affect an outstanding
// cursor — resume must still serve the frozen page.
func TestQueryResume_SurvivesRepoRename(t *testing.T) {
	m, ri, ctx := newRenameTestRepo(t)
	const n = 25 // > defaultPageSize (20) so a cursor is returned
	seedFedMany(t, ctx, n, "Alpha", "alpha body ", "store")

	// Mint the cursor while the repo is still named "alpha".
	first := runQuery(t, ctx, map[string]any{"type": []any{"policy"}})
	require.NotNil(t, first.Cursor, "multi-page query must return a cursor")
	require.Len(t, first.Facts, defaultPageSize, "first page must be full")

	// Rename through the real production path. RenameRepo updates ri in
	// place (same *RepoInstance, new name) — ctx still refers to it, so the
	// next handler call rebuilds a Binding from the SAME instance, now under
	// its new name, exactly as a live server would after a rename lands.
	require.NoError(t, m.RenameRepo("alpha", "alpha-renamed"))
	require.Equal(t, "alpha-renamed", ri.Name(), "rename must update the instance in place")

	// Resume: the rebuilt binding's Name() differs from the one at mint time,
	// but its PinID() (repo:<uid>) does not — the resume must succeed.
	second := runQuery(t, ctx, map[string]any{"cursor": *first.Cursor})
	require.Equal(t, n-defaultPageSize, len(second.Facts), "resumed page must survive the rename")
	require.False(t, second.HasMore, "no more results after the last page")
}
