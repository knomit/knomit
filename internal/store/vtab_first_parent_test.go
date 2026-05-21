//go:build sqlite_vtable

package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVTabRepoRegistry_BindLookupUnbind(t *testing.T) {
	// Sentinel repoHandler value: zero-valued struct is fine for identity testing.
	rh := &repoHandler{}
	const path = "/tmp/vtab_registry_test.db"

	require.Nil(t, lookupVTabRepo(path), "lookup before bind must return nil")

	bindVTabRepo(path, rh)
	require.Same(t, rh, lookupVTabRepo(path), "lookup after bind must return the same handle")

	unbindVTabRepo(path)
	require.Nil(t, lookupVTabRepo(path), "lookup after unbind must return nil")
}

// TestFirstParentChainModule_End2End opens a real store, walks a small
// branch history through the vtab, and asserts the streamed (hash, depth)
// rows match the expected first-parent line.
func TestFirstParentChainModule_End2End(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Build a small history.
	c1, err := svc.Facts().WriteFact(ctx, branch, "kb/a.md", testFactBody("a", 0.5, nil), "init a", "")
	require.NoError(t, err)
	c2, err := svc.Facts().WriteFact(ctx, branch, "kb/b.md", testFactBody("b", 0.5, nil), "init b", "")
	require.NoError(t, err)
	c3, err := svc.Facts().WriteFact(ctx, branch, "kb/c.md", testFactBody("c", 0.5, nil), "init c", "")
	require.NoError(t, err)

	// Query the vtab directly from the shared db handle.
	rh := svc.si.rh
	rows, err := rh.db.QueryContext(ctx,
		`SELECT commit_hash, depth FROM first_parent_chain(?) ORDER BY depth ASC`,
		c3.CommitHash,
	)
	require.NoError(t, err)
	defer rows.Close()

	type row struct {
		hash  string
		depth int
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.hash, &r.depth))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())

	// We expect at least c3, c2, c1 in first-parent order at depths 0..2.
	require.GreaterOrEqual(t, len(got), 3)
	require.Equal(t, c3.CommitHash, got[0].hash)
	require.Equal(t, 0, got[0].depth)
	require.Equal(t, c2.CommitHash, got[1].hash)
	require.Equal(t, 1, got[1].depth)
	require.Equal(t, c1.CommitHash, got[2].hash)
	require.Equal(t, 2, got[2].depth)
}
