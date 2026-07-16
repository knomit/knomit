package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// openTestRegistry opens a LensRegistry in a temp dir and closes it on cleanup.
func openTestRegistry(t *testing.T) *LensRegistry {
	t.Helper()
	r, err := OpenLensRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestLensRegistry_OpenEmptyListsZero(t *testing.T) {
	r := openTestRegistry(t)
	lenses, err := r.List()
	require.NoError(t, err)
	require.Empty(t, lenses)
}

// The schema is created with IF NOT EXISTS: reopening the same file works.
func TestLensRegistry_ReopenSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	r1, err := OpenLensRegistry(path)
	require.NoError(t, err)
	require.NoError(t, r1.Close())

	r2, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer r2.Close()
	lenses, err := r2.List()
	require.NoError(t, err)
	require.Empty(t, lenses)
}
