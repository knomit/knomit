package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// openTestRepoRegistry opens a Registry in a temp dir and closes it on
// cleanup. Named distinctly from lens_test.go's openTestRegistry, which
// returns a *LensRegistry — same package, different tenant under test.
func openTestRepoRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRegistry_InsertAndGet(t *testing.T) {
	r := openTestRepoRegistry(t)
	rec := RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 100}
	require.NoError(t, r.Insert(rec))

	got, ok, err := r.Get("u1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "alpha", got.Name)
	require.Equal(t, StateActive, got.State)
	require.Equal(t, "code", got.Profile)
	require.Empty(t, got.RepoID)
}

// Re-activating into a name an active repo already holds must say WHICH name.
// SetState knows only a uid, so the row's own name has to be read back —
// otherwise the 409 reads `repo already exists: ""` and tells the operator
// nothing. Reachable through Manager.Restore when the active holder is
// registered but its .db is missing, so it is absent from m.repos and the
// in-memory pre-check does not catch it.
func TestRegistry_SetStateNamesTheCollidingRepo(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, r.Insert(RepoRecord{UID: "u2", Name: "alpha", State: StateArchived, Profile: "code", CreatedAt: 2, ArchivedAt: 3}))

	err := r.SetState("u2", StateActive, 0)
	require.ErrorIs(t, err, ErrRepoExists)
	require.Contains(t, err.Error(), `"alpha"`, "the message must name the repo in the way")
}

// An archived repo does NOT reserve its name — the partial index covers active
// rows only, which is how Restore(archiveID, newName) resolves collisions.
func TestRegistry_ArchivedNameIsFree(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.ErrorIs(t, r.Insert(RepoRecord{UID: "u2", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 2}), ErrRepoExists)

	require.NoError(t, r.SetState("u1", StateArchived, 50))
	require.NoError(t, r.Insert(RepoRecord{UID: "u2", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 2}))

	got, ok, err := r.ByName("alpha")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "u2", got.UID, "ByName resolves the ACTIVE holder")
}

// One local copy per knowledge base: repo_id is unique among active repos.
func TestRegistry_RepoIDUniqueAmongActive(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, r.Insert(RepoRecord{UID: "u2", Name: "beta", State: StateActive, Profile: "code", CreatedAt: 2}))

	require.NoError(t, r.RecordRepoID("u1", "root-abc"))
	require.ErrorIs(t, r.RecordRepoID("u2", "root-abc"), ErrRepoAlreadyRegistered)

	// Archiving the holder frees the identity.
	require.NoError(t, r.SetState("u1", StateArchived, 50))
	require.NoError(t, r.RecordRepoID("u2", "root-abc"))
}

// repo_id is mutable: a disjoint-history connect swaps the store and with it
// the root commit. Re-recording the SAME id must also be a no-op, since every
// boot records it.
func TestRegistry_RecordRepoIDIsIdempotentAndUpdatable(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, r.RecordRepoID("u1", "root-abc"))
	require.NoError(t, r.RecordRepoID("u1", "root-abc"))
	require.NoError(t, r.RecordRepoID("u1", "root-xyz"))

	got, _, err := r.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "root-xyz", got.RepoID)
}

// The registry shares control.db with the lens tenant; both open their own
// handle to the same file.
func TestRegistry_SharesControlDBWithLenses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	reg, err := OpenLensRegistry(path)
	require.NoError(t, err)
	defer reg.Close()

	r, err := OpenRegistry(path)
	require.NoError(t, err)
	defer r.Close()

	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	// The lens names the repo by uid, and its foreign key resolves against the
	// repos table the OTHER handle created — proof the two tenants share a file.
	_, err = reg.Create(Lens{Name: "l1", WriteUID: "u1", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
}

func TestRegistry_ListSortedByName(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u2", Name: "zulu", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 2}))
	require.NoError(t, r.Insert(RepoRecord{UID: "u3", Name: "gone", State: StateArchived, Profile: "code", CreatedAt: 3}))

	active, err := r.List(StateActive)
	require.NoError(t, err)
	require.Len(t, active, 2)
	require.Equal(t, "alpha", active[0].Name)
	require.Equal(t, "zulu", active[1].Name)

	archived, err := r.List(StateArchived)
	require.NoError(t, err)
	require.Len(t, archived, 1)
}

func TestRegistry_IsEmpty(t *testing.T) {
	r := openTestRepoRegistry(t)
	empty, err := r.IsEmpty()
	require.NoError(t, err)
	require.True(t, empty)

	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	empty, err = r.IsEmpty()
	require.NoError(t, err)
	require.False(t, empty)
}

// Profile is keyed by uid, so it survives a change of knowledge base — which
// is what a disjoint-history connect does. Keyed by repo_id it silently
// reverted to the default.
func TestRegistry_ProfileSurvivesIdentityChange(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: ProfileCode, CreatedAt: 1}))
	require.NoError(t, r.SetProfile("u1", ProfileChat))
	require.NoError(t, r.RecordRepoID("u1", "root-abc"))
	require.NoError(t, r.RecordRepoID("u1", "root-xyz")) // adopted a remote

	rec, _, err := r.Get("u1")
	require.NoError(t, err)
	require.Equal(t, ProfileChat, rec.Profile)
}

func TestRegistry_SetProfileRejectsUnknown(t *testing.T) {
	r := openTestRepoRegistry(t)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: ProfileCode, CreatedAt: 1}))
	require.ErrorIs(t, r.SetProfile("u1", "nonsense"), ErrInvalidProfile)
}
