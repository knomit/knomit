package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuildGraph_WritesEdgePerRefEvent verifies that after a full
// rebuild, the total DERIVED_FROM edge count equals the total number of
// ref-events in commit_log (added/modified rows × number of local refs
// per blob). Two D versions both ref'ing E should produce 2 edges.
func TestRebuildGraph_WritesEdgePerRefEvent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Two versions of D, both ref'ing E.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "init d", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d v2", 0.85, []string{"kb/e.md"}), "update d", "")
	require.NoError(t, err)

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	si := svc.Search().(*searchIndex)
	var count int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&count))
	require.Equal(t, 2, count, "expected one DERIVED_FROM edge per (D version) referencing E")
}
