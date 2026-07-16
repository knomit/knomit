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

func TestLensRegistry_CreateNormalizesAndGetRoundTrips(t *testing.T) {
	r := openTestRegistry(t)

	stored, err := r.Create(Lens{
		Name:  "eng",
		Write: "scratch",
		Reads: []LensRead{
			{Repo: "product", Branch: "agent/laptop", Source: "prod-src"},
			{Repo: "product", Branch: "ignored-duplicate"}, // dup: first wins
			{Repo: "shared"},
		},
		CreatedAt: 100,
		UpdatedAt: 200,
	})
	require.NoError(t, err)

	// Normalized: deduped by repo, write repo implicitly included, sorted.
	require.Equal(t, []LensRead{
		{Repo: "product", Branch: "agent/laptop", Source: "prod-src"},
		{Repo: "scratch"},
		{Repo: "shared"},
	}, stored.Reads)
	require.Equal(t, int64(100), stored.CreatedAt)
	require.Equal(t, int64(200), stored.UpdatedAt)

	got, ok, err := r.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored, got)
}

// An explicit read entry for the write repo keeps its configured branch.
func TestLensRegistry_WriteRepoExplicitReadKeepsBranch(t *testing.T) {
	r := openTestRegistry(t)
	stored, err := r.Create(Lens{
		Name:  "solo",
		Write: "work",
		Reads: []LensRead{{Repo: "work", Branch: "agent/here"}},
	})
	require.NoError(t, err)
	require.Equal(t, []LensRead{{Repo: "work", Branch: "agent/here"}}, stored.Reads)
}

func TestLensRegistry_CreateValidation(t *testing.T) {
	r := openTestRegistry(t)

	_, err := r.Create(Lens{Name: "", Write: "w"})
	require.ErrorIs(t, err, ErrLensNameEmpty)

	_, err = r.Create(Lens{Name: "x", Write: ""})
	require.ErrorIs(t, err, ErrLensWriteEmpty)

	_, err = r.Create(Lens{Name: "dup", Write: "w"})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "dup", Write: "other"})
	require.ErrorIs(t, err, ErrLensExists)
}

func TestLensRegistry_GetAbsent(t *testing.T) {
	r := openTestRegistry(t)
	_, ok, err := r.Get("nope")
	require.NoError(t, err)
	require.False(t, ok)
}
