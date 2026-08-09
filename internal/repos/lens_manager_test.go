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
	require.Nil(t, m.LensRegistry())
}

func TestManager_Registry_OpenAfterStartAndUsable(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
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
	data, err := os.ReadFile(m.RepoPath(srcRI.UID()))
	require.NoError(t, err)
	dstPath := filepath.Join(reposDir, dst+".db")
	require.NoError(t, os.WriteFile(dstPath, data, 0o644))
	require.NoError(t, m.Add(dst, "", dstPath, nil))
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
		Name: "lens_ok", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
}

func TestManager_ValidateLens_UnknownRepo(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "lens_unknownrepo", Write: "alpha", Reads: []LensRead{{Repo: "ghost"}},
	})
	require.ErrorIs(t, err, ErrRepoNotFound)
	require.ErrorContains(t, err, "ghost")
}

func TestManager_ValidateLens_ReplicaRejected(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	cloneLensRepo(t, m, "alpha", "alpha_clone")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "lens_replica", Write: "alpha", Reads: []LensRead{{Repo: "alpha_clone"}},
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
		Name: "lens_unknownbranch", Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: "nope"}},
	})
	require.ErrorIs(t, err, ErrLensBranchUnknown)
	require.ErrorContains(t, err, "nope")
}

// TestManager_ValidateLens_LookupFailureNotUnknownBranch pins the classification
// split: a branch lookup that fails for a NON-missing reason (here a cancelled
// context) must not be reported as ErrLensBranchUnknown. That sentinel maps to a
// 4xx and would wrongly blame the caller's lens spec for a transient store
// failure. The pinned branch ("main") genuinely exists, so cancellation is the
// only possible cause of failure — making the distinction load-bearing rather
// than incidental.
func TestManager_ValidateLens_LookupFailureNotUnknownBranch(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // fail the branch lookup for a non-missing reason

	err := m.ValidateLens(ctx, Lens{
		Name: "lens_lookupfail", Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: "main"}},
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrLensBranchUnknown)
	require.ErrorIs(t, err, context.Canceled)
}

func TestManager_ValidateLens_EmptyBranchOK(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "lens_emptybranch", Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: ""}},
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

	got, ok, err := m.LensRegistry().Get("eng")
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
	_, ok, err := m.LensRegistry().Get("bad")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestManager_CreateLens_RejectsEmptyWrite(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	// An empty write repo must surface ErrLensWriteEmpty (→ 400), not the
	// ErrRepoNotFound it used to hit via m.Get("") (→ 422) (A1).
	_, err := m.CreateLens(context.Background(), Lens{Name: "eng", Write: ""})
	require.ErrorIs(t, err, ErrLensWriteEmpty)

	_, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.False(t, ok, "a rejected lens must not persist")
}

// checkMemberIDCollision is unit-tested directly with synthetic IDs because real
// repos with colliding 12-hex prefixes cannot be manufactured (A2).
func TestCheckMemberIDCollision(t *testing.T) {
	const (
		idA = "aaaaaaaaaaaa0000000000000000000000000000"
		idB = "bbbbbbbbbbbb1111111111111111111111111111"
		// Shares only the first 12 hex with idA — distinct full IDs, same
		// routing prefix Binding.ByID uses.
		idAPrefixTwin = "aaaaaaaaaaaa9999999999999999999999999999"
	)

	t.Run("no collision", func(t *testing.T) {
		require.NoError(t, checkMemberIDCollision(map[string]string{"a": idA, "b": idB}))
	})

	t.Run("full-ID collision (true replica)", func(t *testing.T) {
		err := checkMemberIDCollision(map[string]string{"a": idA, "clone": idA})
		require.ErrorIs(t, err, ErrReplicaInLens)
		require.ErrorContains(t, err, "a")
		require.ErrorContains(t, err, "clone")
		require.ErrorContains(t, err, idA[:12])
	})

	t.Run("12-hex-prefix-only collision", func(t *testing.T) {
		err := checkMemberIDCollision(map[string]string{"a": idA, "twin": idAPrefixTwin})
		require.ErrorIs(t, err, ErrReplicaInLens)
		require.ErrorContains(t, err, idA[:12])
	})
}

func TestManager_ValidateLens_RejectsInvalidName(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "Bad Name", Write: "alpha",
	})
	require.ErrorIs(t, err, ErrInvalidLensName)
	require.ErrorContains(t, err, "Bad Name")
}

func TestManager_ValidateLens_RejectsEmptyName(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "", Write: "alpha",
	})
	require.ErrorIs(t, err, ErrInvalidLensName)
}

func TestManager_ValidateLens_RejectsRepoNameCollision(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "beta", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.ErrorIs(t, err, ErrLensNameConflictsRepo)
	require.ErrorContains(t, err, "beta")
}

func TestManager_ValidateLens_AcceptsDistinctName(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	err := m.ValidateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
}

func TestManager_CreateLens_RejectsRepoNameCollision(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")

	_, err := m.CreateLens(context.Background(), Lens{
		Name: "beta", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.ErrorIs(t, err, ErrLensNameConflictsRepo)

	// A rejected lens must not have been persisted.
	_, ok, err := m.LensRegistry().Get("beta")
	require.NoError(t, err)
	require.False(t, ok)
}

// Reverse M-1 direction: a repo may not be created under a name a lens already
// holds. Mirrors TestManager_ValidateLens_RejectsRepoNameCollision (forward).
func TestCreatePreflight_RejectsLensNameCollision(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	_, err := m.LensRegistry().Create(Lens{Name: "eng", Write: "alpha", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	err = m.CreatePreflight(CreateSpec{Name: "eng", Mode: "preset", OntologyPreset: "default"})
	require.ErrorIs(t, err, ErrRepoNameConflictsLens)
	require.ErrorContains(t, err, "eng")
}

func TestCreate_RejectsLensNameCollision(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	_, err := m.LensRegistry().Create(Lens{Name: "eng", Write: "alpha", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	_, err = m.Create(context.Background(), CreateSpec{
		Name: "eng", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrRepoNameConflictsLens)

	// A rejected create must not have registered a repo.
	require.Nil(t, m.Get("eng"))
}

func TestCreate_AcceptsNameMatchingNoLens(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	_, err := m.LensRegistry().Create(Lens{Name: "eng", Write: "alpha", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// "sales" collides with no lens, so creation still succeeds.
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "sales", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.NotNil(t, m.Get("sales"))
}

func TestRestore_RejectsNameTakenByLens(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "work")

	// Archive "work", THEN mint a lens named "work" — ValidateLens's active-only
	// check lets this through because no active repo "work" remains.
	info, err := m.Archive("work")
	require.NoError(t, err)
	_, err = m.LensRegistry().Create(Lens{Name: "work", Write: "alpha", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// Restoring "work" back to active must refuse the now-taken name.
	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrRepoNameConflictsLens)

	// The archive must remain intact so the repo stays recoverable.
	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Nil(t, m.Get("work"))
}

func TestArchive_BlockedWhileLensReferencesRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	_, err = m.LensRegistry().Create(Lens{Name: "eng", Write: "work", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	_, err = m.Archive("work")
	require.ErrorIs(t, err, ErrRepoInUseByLens)
	require.NotNil(t, m.Get("work"), "repo must stay registered when the guard blocks")

	// Deleting the lens unblocks archiving.
	require.NoError(t, m.LensRegistry().Delete("eng"))
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
	_, err = m.LensRegistry().Create(Lens{Name: "eng", Write: "work", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	err = m.Purge(info.ID)
	require.ErrorIs(t, err, ErrRepoInUseByLens)

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1, "blocked purge must leave the archive intact")

	// Deleting the lens unblocks purging.
	require.NoError(t, m.LensRegistry().Delete("eng"))
	require.NoError(t, m.Purge(info.ID))
	left, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, left)
}
