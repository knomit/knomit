package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// TestReservedRepoNameIsRefused pins the reservation itself. "control" is a
// grammatically perfect repo name — lowercase letters only — so nothing in the
// character rules can catch it; only the reserved set can.
func TestReservedRepoNameIsRefused(t *testing.T) {
	for _, name := range []string{"control"} {
		require.False(t, IsValidName(name),
			"%q names a database knomit already owns; a repo of that name collides with it in the replica namespace", name)
	}
	// Neighbours must still be usable — the reservation is an exact-name rule,
	// not a prefix or substring one.
	for _, name := range []string{"controls", "control-plane", "my-control", "kontrol"} {
		require.True(t, IsValidName(name), "%q is not reserved and must stay creatable", name)
	}
}

// TestRepoNameGrammarRejectsPathSeparators pins the property the reserved set is
// allowed to LEAN ON rather than restate: a name is interpolated straight into
// <home>/repos/<name>.db and into the replica key, and the archive namespace is
// kept disjoint by the fact that "archive/<id>" contains a slash. If the grammar
// ever admitted a separator, both would silently stop holding.
func TestRepoNameGrammarRejectsPathSeparators(t *testing.T) {
	for _, name := range []string{
		"a/b", "../../etc/passwd", "..", ".", `a\b`, "archive/abc", "control.db",
		"repos/core", "a b", "A", "",
	} {
		require.False(t, IsValidName(name), "name %q must be refused", name)
	}
}

// TestCreateRefusesReservedName is the API-visible half: Create and its
// preflight must both refuse, and with ErrInvalidName, which the REST layer maps
// to 400 rather than to a 500 from somewhere deeper.
func TestCreateRefusesReservedName(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	tracker := &fakeBackupTracker{}
	// The control database is already tracked, exactly as cmd/serve tracks it at
	// boot. Without the reservation, the Create below would build the repo and
	// then log a tracker refusal — leaving it live and unreplicated.
	require.NoError(t, tracker.Track("control", filepath.Join(dir, "control.db")))

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
		Backup:      tracker,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	spec := CreateSpec{Name: "control", Mode: "preset", OntologyPreset: "default"}
	require.ErrorIs(t, m.CreatePreflight(spec), ErrInvalidName)

	ri, err := m.Create(context.Background(), spec, nil)
	require.ErrorIs(t, err, ErrInvalidName)
	require.Nil(t, ri)

	// And nothing was left behind: no repo, and above all no second database
	// claiming the control name in the replica.
	require.Nil(t, m.Get("control"))
	require.Equal(t, filepath.Join(dir, "control.db"), tracker.trackedPath("control"),
		"the control database's replication must still point at <home>/control.db")
}

// TestStartSkipsAReservedRegistryRowInsteadOfRefusingToBoot covers the instance
// that already has the bad row — created before the reservation existed, or by
// hand. The recovery must be a boot that comes up with that one repo missing and
// logged, NOT a refusal: a refusal can only be cleared by editing control.db.
func TestStartSkipsAReservedRegistryRowInsteadOfRefusingToBoot(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	reg, err := OpenRepoRegistry(filepath.Join(dir, "control.db"))
	require.NoError(t, err)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "control", State: RepoActive}))
	require.NoError(t, reg.Close())

	// With a database file present too, so the row is one Start would otherwise
	// OPEN. A row with no file is skipped for an unrelated reason ("no database
	// and no origin"), which would make this pass whether the name is reserved
	// or not.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "repos"), 0o755))
	svc, err := store.Open(filepath.Join(dir, "repos", "control.db"))
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "machine/test"))
	require.NoError(t, svc.Close())

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
	})
	require.NoError(t, m.Start(), "a reserved registry row must not be able to stop the server booting")
	t.Cleanup(func() { _ = m.Close() })
	require.Nil(t, m.Get("control"), "the reserved row must be skipped, not opened")
}

// TestCreateLensRefusesReservedName: lens names share the grammar with repos so
// the two namespaces stay disjoint (gotcha M-1), and a lens is registered under
// its name in the same control.db. Leaving the reservation to the repo path only
// would let a lens claim the name the repo path just refused.
func TestCreateLensRefusesReservedName(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.CreateLens(context.Background(), Lens{Name: "control", Write: "whatever"})
	require.True(t, errors.Is(err, ErrInvalidLensName), "CreateLens(%q) = %v, want ErrInvalidLensName", "control", err)
}
