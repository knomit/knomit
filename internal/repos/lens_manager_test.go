package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

func TestManager_Registry_NilBeforeStart(t *testing.T) {
	m := New(context.Background(), Deps{Cfg: config.Config{}, AgentBranch: "agent/test"})
	require.Nil(t, m.Registry())
}

func TestManager_Registry_OpenAfterStartAndUsable(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()
	require.NotNil(t, reg)

	// control.db lives at <home>/control.db (NOT under repos/).
	// newLifecycleManager sets Cfg.Home; recover it from the manager's deps.
	_, err := os.Stat(filepath.Join(m.deps.Cfg.Home, "control.db"))
	require.NoError(t, err)

	stored, err := reg.Create(Lens{Name: "eng", Write: "core", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	got, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored, got)
}

// makeLensRepo creates a preset repo under the manager and returns its instance.
func makeLensRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: name, Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
	return ri
}

// cloneLensRepo provisions a replica of an existing active repo by copying its
// .db file to a new name, so both instances share the same root-commit ID
// (RFC decision 11). The source WAL is checkpointed first so the copied .db is
// self-contained, then the copy is registered under dst.
func cloneLensRepo(t *testing.T, m *Manager, src, dst string) *RepoInstance {
	t.Helper()
	srcRI := m.Get(src)
	require.NotNil(t, srcRI)
	srcRI.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc)
		require.NoError(t, svc.Checkpoint())
	})
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	data, err := os.ReadFile(filepath.Join(reposDir, src+".db"))
	require.NoError(t, err)
	dstPath := filepath.Join(reposDir, dst+".db")
	require.NoError(t, os.WriteFile(dstPath, data, 0o644))
	require.NoError(t, m.Add(dst, dstPath))
	ri := m.Get(dst)
	require.NotNil(t, ri)
	// The copy must resolve to the SAME id as its source — that's what makes it
	// a replica the validator must reject.
	require.Equal(t, srcRI.ID(), ri.ID())
	require.NotEmpty(t, ri.ID())
	return ri
}

func TestManager_ValidateLens_OK(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
}

func TestManager_ValidateLens_UnknownRepo(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	err := m.ValidateLens(context.Background(), Lens{
		Write: "alpha", Reads: []LensRead{{Repo: "ghost"}},
	})
	require.ErrorIs(t, err, ErrRepoNotFound)
	require.ErrorContains(t, err, "ghost")
}

func TestManager_ValidateLens_ReplicaRejected(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	cloneLensRepo(t, m, "alpha", "alpha_clone")

	err := m.ValidateLens(context.Background(), Lens{
		Write: "alpha", Reads: []LensRead{{Repo: "alpha_clone"}},
	})
	require.ErrorIs(t, err, ErrReplicaInLens)
	// Map iteration order is random — the pair may be named in either order.
	require.ErrorContains(t, err, "alpha")
	require.ErrorContains(t, err, "alpha_clone")
}

func TestManager_ValidateLens_UnknownBranch(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: "nope"}},
	})
	require.ErrorIs(t, err, ErrLensBranchUnknown)
	require.ErrorContains(t, err, "nope")
}

func TestManager_ValidateLens_EmptyBranchOK(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: ""}},
	})
	require.NoError(t, err)
}

func TestManager_CreateLens_PersistsAfterValidation(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	stored, err := m.CreateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
	require.Equal(t, "eng", stored.Name)

	got, ok, err := m.Registry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored, got)
}

func TestManager_CreateLens_RejectsInvalid(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	cloneLensRepo(t, m, "alpha", "alpha_clone")

	_, err := m.CreateLens(context.Background(), Lens{
		Name: "bad", Write: "alpha", Reads: []LensRead{{Repo: "alpha_clone"}},
	})
	require.ErrorIs(t, err, ErrReplicaInLens)

	// A rejected lens must not have been persisted.
	_, ok, err := m.Registry().Get("bad")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestArchive_BlockedWhileLensReferencesRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	_, err = m.Registry().Create(Lens{Name: "eng", Write: "work", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	_, err = m.Archive("work")
	require.ErrorIs(t, err, ErrRepoInUseByLens)
	require.NotNil(t, m.Get("work"), "repo must stay registered when the guard blocks")

	// Deleting the lens unblocks archiving.
	require.NoError(t, m.Registry().Delete("eng"))
	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, "work", info.Name)
}

func TestPurge_BlockedWhileLensReferencesArchivedRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	// Archive first (no lens yet), then reference the archived repo by name.
	info, err := m.Archive("work")
	require.NoError(t, err)
	_, err = m.Registry().Create(Lens{Name: "eng", Write: "work", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	err = m.Purge(info.ID)
	require.ErrorIs(t, err, ErrRepoInUseByLens)

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1, "blocked purge must leave the archive intact")

	// Deleting the lens unblocks purging.
	require.NoError(t, m.Registry().Delete("eng"))
	require.NoError(t, m.Purge(info.ID))
	left, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, left)
}
