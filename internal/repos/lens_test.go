package repos

import (
	"database/sql"
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

func TestLensRegistry_ListReturnsPopulatedLensesSorted(t *testing.T) {
	r := openTestRegistry(t)
	_, err := r.Create(Lens{Name: "zeta", Write: "work", Reads: []LensRead{{Repo: "shared", Branch: "agent/x", Source: "shared-src"}}, CreatedAt: 5, UpdatedAt: 6})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "alpha", Write: "other", CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)

	lenses, err := r.List()
	require.NoError(t, err)
	require.Len(t, lenses, 2)
	require.Equal(t, "alpha", lenses[0].Name) // sorted by name
	require.Equal(t, []LensRead{{Repo: "other"}}, lenses[0].Reads)
	require.Equal(t, int64(1), lenses[0].CreatedAt)
	require.Equal(t, "zeta", lenses[1].Name)
	require.Equal(t, []LensRead{
		{Repo: "shared", Branch: "agent/x", Source: "shared-src"},
		{Repo: "work"},
	}, lenses[1].Reads)
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

// Description round-trips through Create → Get → List. It is display metadata,
// so normalize() must leave it untouched.
func TestLensRegistry_DescriptionRoundTrips(t *testing.T) {
	r := openTestRegistry(t)
	stored, err := r.Create(Lens{
		Name:        "docs",
		Write:       "work",
		Description: "A **markdown** description.",
		Reads:       []LensRead{{Repo: "shared"}},
		CreatedAt:   1, UpdatedAt: 2,
	})
	require.NoError(t, err)
	require.Equal(t, "A **markdown** description.", stored.Description)

	got, ok, err := r.Get("docs")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "A **markdown** description.", got.Description)

	lenses, err := r.List()
	require.NoError(t, err)
	require.Len(t, lenses, 1)
	require.Equal(t, "A **markdown** description.", lenses[0].Description)
}

// A control.db created by the OLD schema (no description column) upgrades in
// place: OpenLensRegistry adds the column, pre-existing rows read back with an
// empty description, and new descriptions round-trip.
func TestLensRegistry_UpgradesOldSchemaInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	// Hand-build the pre-description schema and seed a row.
	old, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	require.NoError(t, err)
	_, err = old.Exec(`
CREATE TABLE lenses (
    name       TEXT PRIMARY KEY,
    write_repo TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO lenses (name, write_repo, created_at, updated_at) VALUES ('legacy', 'work', 3, 4)`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO lens_reads (lens_name, repo, branch) VALUES ('legacy', 'work', '')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	// Open through the registry: the ALTER runs, upgrading in place.
	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	got, ok, err := r.Get("legacy")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "", got.Description) // pre-existing row, default ''

	// A new lens with a description round-trips after the upgrade.
	_, err = r.Create(Lens{Name: "fresh", Write: "work", Description: "new", CreatedAt: 5, UpdatedAt: 6})
	require.NoError(t, err)
	got, ok, err = r.Get("fresh")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "new", got.Description)
}

func TestLensRegistry_GetAbsent(t *testing.T) {
	r := openTestRegistry(t)
	_, ok, err := r.Get("nope")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLensRegistry_RefsRepo(t *testing.T) {
	r := openTestRegistry(t)
	_, err := r.Create(Lens{Name: "a", Write: "work", Reads: []LensRead{{Repo: "shared"}}})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "b", Write: "other", Reads: []LensRead{{Repo: "work"}}})
	require.NoError(t, err)

	refs, err := r.RefsRepo("work")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, refs) // write ref (a) + read ref (b), sorted, deduped

	refs, err = r.RefsRepo("shared")
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, refs)

	refs, err = r.RefsRepo("unreferenced")
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestLensRegistry_DeleteIdempotentAndCascades(t *testing.T) {
	r := openTestRegistry(t)
	_, err := r.Create(Lens{Name: "gone", Write: "work", Reads: []LensRead{{Repo: "shared"}}})
	require.NoError(t, err)

	require.NoError(t, r.Delete("gone"))
	require.NoError(t, r.Delete("gone")) // idempotent: absent is not an error

	// Cascade: no read rows survive, so nothing references the repos anymore.
	refs, err := r.RefsRepo("shared")
	require.NoError(t, err)
	require.Empty(t, refs)

	// The name is reusable after delete (old rows fully gone).
	_, err = r.Create(Lens{Name: "gone", Write: "other"})
	require.NoError(t, err)
}
