package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestRenameLens_RekeysWithoutDeleteAndRecreate is the whole point of giving
// lenses a uid: renaming a lens must be a display-string UPDATE, never a
// delete-and-recreate, because an in-flight MCP cursor is pinned to
// lens:<uid> (Binding.PinID(), RFC §7.3) and would be orphaned by anything
// that mints a fresh uid. The uid-unchanged assertion below is what actually
// proves that — a rename that silently deleted and recreated the row would
// pass every other assertion in this file but fail this one.
func TestRenameLens_RekeysWithoutDeleteAndRecreate(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")
	beta := makeLensRepo(t, m, "beta")

	stored, err := m.CreateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: beta.UID()}},
	})
	require.NoError(t, err)

	require.NoError(t, m.RenameLens("eng", "engineering"))

	_, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.False(t, ok, "the old name must no longer resolve")

	got, ok, err := m.LensRegistry().Get("engineering")
	require.NoError(t, err)
	require.True(t, ok, "the new name must resolve")
	require.Equal(t, stored.UID, got.UID, "uid is identity and must never change across a rename")
	require.Equal(t, "engineering", got.Name)
	require.Equal(t, stored.WriteUID, got.WriteUID)
	// Read mounts are untouched: lens_reads references lens_uid, which the
	// rename never rewrites (Task 4b's whole point).
	require.Equal(t, stored.Reads, got.Reads, "read mounts must survive a rename unchanged")
}

func TestRenameLens_RejectsInvalidName(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")
	_, err := m.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)

	require.ErrorIs(t, m.RenameLens("eng", "Has Capitals"), ErrInvalidLensName)
	require.ErrorIs(t, m.RenameLens("eng", ""), ErrInvalidLensName)

	_, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok, "a rejected rename leaves the lens alone")
}

func TestRenameLens_RejectsNameHeldByAnotherLens(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")

	_, err := m.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)
	_, err = m.CreateLens(context.Background(), Lens{Name: "sales", WriteUID: alpha.UID()})
	require.NoError(t, err)

	require.ErrorIs(t, m.RenameLens("eng", "sales"), ErrLensExists)

	_, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok, "a rejected rename leaves the source lens alone")
	_, ok, err = m.LensRegistry().Get("sales")
	require.NoError(t, err)
	require.True(t, ok, "the colliding lens is untouched")
}

// A repo may hold newName even though no lens does — repos and lenses share
// one namespace (gotcha M-1). Same guard CreateLens/UpdateLens run via
// validateLensLocked; RenameLens must run it too.
func TestRenameLens_RejectsNameHeldByRepo(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "gamma") // an active repo named "gamma"

	_, err := m.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)

	require.ErrorIs(t, m.RenameLens("eng", "gamma"), ErrLensNameConflictsRepo)

	_, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok, "a rejected rename leaves the lens alone")
}

func TestRenameLens_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")
	_, err := m.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)

	require.ErrorIs(t, m.RenameLens("ghost", "engineering"), ErrLensNotFound)
}

// Renaming to the current name is a successful no-op, not a self-collision
// against the lens's own row.
func TestRenameLens_SameNameIsNoOp(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")
	stored, err := m.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)

	require.NoError(t, m.RenameLens("eng", "eng"))

	got, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored.UID, got.UID)
}

// TestRenameLens_PersistsAcrossRestart pins the durable half: the UPDATE to
// control.db, not just a read served from a connection that never left
// memory. An implementation that renamed only some in-memory copy — or that
// (per the landmine this task was warned about) routed the rename through
// LensRegistry.Update, which resolves its target by NAME and would silently
// no-op or misbehave against a changed name — would need a fresh process
// re-reading the registry from disk to be caught; a same-process check alone
// is not enough, because a stale connection could still serve the old value
// from a cache the driver never invalidated.
func TestRenameLens_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	boot := func() *Manager {
		m := New(context.Background(), Deps{
			Cfg:                   config.Config{Home: dir},
			AgentBranch:           "machine/test",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		return m
	}

	m1 := boot()
	alpha := makeLensRepo(t, m1, "alpha")
	_, err := m1.CreateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.NoError(t, err)
	require.NoError(t, m1.RenameLens("eng", "engineering"))
	require.NoError(t, m1.Close())

	m2 := boot()
	t.Cleanup(func() { _ = m2.Close() })

	_, ok, err := m2.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.False(t, ok, "the old name must not resurrect on reboot")

	got, ok, err := m2.LensRegistry().Get("engineering")
	require.NoError(t, err)
	require.True(t, ok, "the rename must have reached the registry row, not only an in-memory read")
	require.Equal(t, alpha.UID(), got.WriteUID)
}
