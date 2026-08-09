package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// openTestRegistry opens a LensRegistry in a temp dir and closes it on cleanup.
//
// It opens the REPOS tenant on the same control.db too: lens membership is
// keyed by repos(uid) and the schema enforces that with a foreign key, so a
// member row must exist before any lens can name it. Members are seeded with
// seedMember.
func openTestRegistry(t *testing.T) (*LensRegistry, *Registry) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	repoReg, err := OpenRegistry(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repoReg.Close() })
	return r, repoReg
}

// seedMember registers a repo and returns its uid — the key lens membership
// speaks. The uid is deliberately spelled UNLIKE the name so a test that hands
// a name to something expecting a uid fails loudly instead of passing by luck.
func seedMember(t *testing.T, r *Registry, name string) string {
	t.Helper()
	uid := "uid-" + name
	require.NoError(t, r.Insert(RepoRecord{
		UID: uid, Name: name, State: StateActive, Profile: ProfileCode, CreatedAt: 1,
	}))
	return uid
}

func TestLensRegistry_OpenEmptyListsZero(t *testing.T) {
	r, _ := openTestRegistry(t)
	lenses, err := r.List()
	require.NoError(t, err)
	require.Empty(t, lenses)
}

func TestLensRegistry_ListReturnsPopulatedLensesSorted(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	shared := seedMember(t, repoReg, "shared")
	other := seedMember(t, repoReg, "other")

	_, err := r.Create(Lens{Name: "zeta", WriteUID: work, Reads: []LensRead{{RepoUID: shared, Branch: "agent/x", Source: "shared-src"}}, CreatedAt: 5, UpdatedAt: 6})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "alpha", WriteUID: other, CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)

	lenses, err := r.List()
	require.NoError(t, err)
	require.Len(t, lenses, 2)
	require.Equal(t, "alpha", lenses[0].Name) // sorted by name
	require.Equal(t, []LensRead{{RepoUID: other}}, lenses[0].Reads)
	require.Equal(t, int64(1), lenses[0].CreatedAt)
	require.Equal(t, "zeta", lenses[1].Name)
	require.Equal(t, []LensRead{
		{RepoUID: shared, Branch: "agent/x", Source: "shared-src"},
		{RepoUID: work},
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

// The lens tables carry foreign keys into repos(uid), a table the lens tenant
// does not create. Opening the lens registry FIRST — the order Manager.Start
// uses — must still succeed: SQLite resolves a foreign key's parent table
// lazily, at write time rather than at CREATE TABLE.
func TestLensRegistry_OpensBeforeRepoRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	r, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer r.Close()

	repoReg, err := OpenRegistry(path)
	require.NoError(t, err)
	defer repoReg.Close()

	uid := seedMember(t, repoReg, "work")
	_, err = r.Create(Lens{Name: "eng", WriteUID: uid, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
}

// A lens may not name a member that is not in the repo registry: the foreign
// key refuses the row. This is what makes uid the real key — a bare string that
// merely looks like one cannot be stored.
func TestLensRegistry_RejectsUnregisteredMember(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")

	_, err := r.Create(Lens{Name: "eng", WriteUID: "uid-nobody", CreatedAt: 1, UpdatedAt: 1})
	require.Error(t, err, "an unregistered write member must be refused")

	_, err = r.Create(Lens{Name: "eng", WriteUID: work, Reads: []LensRead{{RepoUID: "uid-nobody"}}, CreatedAt: 1, UpdatedAt: 1})
	require.Error(t, err, "an unregistered read member must be refused")

	// Neither attempt left a row behind.
	_, ok, err := r.Get("eng")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLensRegistry_CreateNormalizesAndGetRoundTrips(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	scratch := seedMember(t, repoReg, "scratch")
	product := seedMember(t, repoReg, "product")
	shared := seedMember(t, repoReg, "shared")

	stored, err := r.Create(Lens{
		Name:     "eng",
		WriteUID: scratch,
		Reads: []LensRead{
			{RepoUID: product, Branch: "agent/laptop", Source: "prod-src"},
			{RepoUID: product, Branch: "ignored-duplicate"}, // dup: first wins
			{RepoUID: shared},
		},
		CreatedAt: 100,
		UpdatedAt: 200,
	})
	require.NoError(t, err)

	// Normalized: deduped by uid, write repo implicitly included, sorted by uid.
	require.Equal(t, []LensRead{
		{RepoUID: product, Branch: "agent/laptop", Source: "prod-src"},
		{RepoUID: scratch},
		{RepoUID: shared},
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
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	stored, err := r.Create(Lens{
		Name:     "solo",
		WriteUID: work,
		Reads:    []LensRead{{RepoUID: work, Branch: "agent/here"}},
	})
	require.NoError(t, err)
	require.Equal(t, []LensRead{{RepoUID: work, Branch: "agent/here"}}, stored.Reads)
}

func TestLensRegistry_CreateValidation(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	w := seedMember(t, repoReg, "w")
	other := seedMember(t, repoReg, "other")

	_, err := r.Create(Lens{Name: "", WriteUID: w})
	require.ErrorIs(t, err, ErrLensNameEmpty)

	_, err = r.Create(Lens{Name: "x", WriteUID: ""})
	require.ErrorIs(t, err, ErrLensWriteEmpty)

	_, err = r.Create(Lens{Name: "dup", WriteUID: w})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "dup", WriteUID: other})
	require.ErrorIs(t, err, ErrLensExists)
}

// Description round-trips through Create → Get → List. It is display metadata,
// so normalize() must leave it untouched.
func TestLensRegistry_DescriptionRoundTrips(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	shared := seedMember(t, repoReg, "shared")
	stored, err := r.Create(Lens{
		Name:        "docs",
		WriteUID:    work,
		Description: "A **markdown** description.",
		Reads:       []LensRead{{RepoUID: shared}},
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

func TestLensRegistry_GetAbsent(t *testing.T) {
	r, _ := openTestRegistry(t)
	_, ok, err := r.Get("nope")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLensRegistry_RefsRepo(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	shared := seedMember(t, repoReg, "shared")
	other := seedMember(t, repoReg, "other")

	_, err := r.Create(Lens{Name: "a", WriteUID: work, Reads: []LensRead{{RepoUID: shared}}})
	require.NoError(t, err)
	_, err = r.Create(Lens{Name: "b", WriteUID: other, Reads: []LensRead{{RepoUID: work}}})
	require.NoError(t, err)

	refs, err := r.RefsRepo(work)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, refs) // write ref (a) + read ref (b), sorted, deduped

	refs, err = r.RefsRepo(shared)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, refs)

	refs, err = r.RefsRepo("uid-unreferenced")
	require.NoError(t, err)
	require.Empty(t, refs)
}

// RefsRepo speaks uids, not names. A member's NAME must match nothing — the
// guard callers (Archive, Purge) would otherwise pass a name, find no refs, and
// permit destroying a live lens member.
func TestLensRegistry_RefsRepo_NameMatchesNothing(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	_, err := r.Create(Lens{Name: "a", WriteUID: work})
	require.NoError(t, err)

	refs, err := r.RefsRepo("work") // the NAME, not the uid
	require.NoError(t, err)
	require.Empty(t, refs, "RefsRepo must not match on repo name")

	refs, err = r.RefsRepo(work)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, refs)
}

// A rename is a repos-table UPDATE; the lens row is keyed by uid and untouched.
func TestLensRegistry_SurvivesMemberRename(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	_, err := r.Create(Lens{Name: "a", WriteUID: work, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	require.NoError(t, repoReg.Rename(work, "work-renamed"))

	got, ok, err := r.Get("a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, work, got.WriteUID)

	refs, err := r.RefsRepo(work)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, refs, "the reference still resolves after the rename")
}

func TestLensRegistry_DeleteIdempotentAndCascades(t *testing.T) {
	r, repoReg := openTestRegistry(t)
	work := seedMember(t, repoReg, "work")
	shared := seedMember(t, repoReg, "shared")
	other := seedMember(t, repoReg, "other")

	_, err := r.Create(Lens{Name: "gone", WriteUID: work, Reads: []LensRead{{RepoUID: shared}}})
	require.NoError(t, err)

	require.NoError(t, r.Delete("gone"))
	require.NoError(t, r.Delete("gone")) // idempotent: absent is not an error

	// Cascade: no read rows survive, so nothing references the repos anymore.
	refs, err := r.RefsRepo(shared)
	require.NoError(t, err)
	require.Empty(t, refs)

	// The name is reusable after delete (old rows fully gone).
	_, err = r.Create(Lens{Name: "gone", WriteUID: other})
	require.NoError(t, err)
}
