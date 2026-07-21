package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------- LensRegistry.Update (registry-layer mechanics) ----------

// namesOf returns the read-mount repo names of a lens, in stored (sorted) order.
func namesOf(reads []LensRead) []string {
	out := make([]string, len(reads))
	for i, r := range reads {
		out[i] = r.Repo
	}
	return out
}

func TestLensRegistry_Update_ReplaceReads(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Create(Lens{Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// Wholesale replace: beta drops out, gamma comes in. normalize folds the
	// write repo (alpha) back in and sorts.
	updated, err := reg.Update(Lens{Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "gamma"}}, CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "gamma"}, namesOf(updated.Reads))

	got, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"alpha", "gamma"}, namesOf(got.Reads), "beta must be gone; gamma present")
}

func TestLensRegistry_Update_ChangeWrite(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Create(Lens{Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	updated, err := reg.Update(Lens{Name: "eng", Write: "gamma", Reads: []LensRead{{Repo: "beta"}}, CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, "gamma", updated.Write)
	require.Equal(t, []string{"beta", "gamma"}, namesOf(updated.Reads), "new write repo folds into reads")

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, "gamma", got.Write)
}

func TestLensRegistry_Update_DescriptionAndReadsRoundTrip(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Create(Lens{Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// Same read set, new description: mounts survive verbatim, description updates.
	updated, err := reg.Update(Lens{Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}}, Description: "team kb", CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, "team kb", updated.Description)
	require.Equal(t, []string{"alpha", "beta"}, namesOf(updated.Reads))

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, "team kb", got.Description)
	require.Equal(t, []string{"alpha", "beta"}, namesOf(got.Reads))
}

func TestLensRegistry_Update_PreservesCreatedAt(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Create(Lens{Name: "eng", Write: "alpha", CreatedAt: 100, UpdatedAt: 100})
	require.NoError(t, err)

	_, err = reg.Update(Lens{Name: "eng", Write: "alpha", CreatedAt: 100, UpdatedAt: 200})
	require.NoError(t, err)

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, int64(100), got.CreatedAt, "created_at is immutable across Update")
	require.Equal(t, int64(200), got.UpdatedAt)
}

func TestLensRegistry_Update_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Update(Lens{Name: "ghost", Write: "alpha", UpdatedAt: 1})
	require.ErrorIs(t, err, ErrLensNotFound)
	require.ErrorContains(t, err, "ghost")
}

func TestLensRegistry_Update_EmptyWrite(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()

	_, err := reg.Update(Lens{Name: "eng", Write: "", UpdatedAt: 1})
	require.ErrorIs(t, err, ErrLensWriteEmpty)
}

// ---------- Manager.UpdateLens (validation parity with CreateLens) ----------

// seedLens makes members and persists a starting lens via the validated path.
func seedLens(t *testing.T, m *Manager) {
	t.Helper()
	makeLensRepo(t, m, "alpha")
	makeLensRepo(t, m, "beta")
	_, err := m.CreateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
}

func TestManager_UpdateLens_PersistsAfterValidation(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)
	makeLensRepo(t, m, "gamma")

	stored, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "gamma"}}, Description: "d",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "gamma"}, namesOf(stored.Reads))
	require.Equal(t, "d", stored.Description)

	got, ok, err := m.Registry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"alpha", "gamma"}, namesOf(got.Reads), "beta replaced by gamma")
}

func TestManager_UpdateLens_UnknownMember(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "ghost"}},
	})
	require.ErrorIs(t, err, ErrRepoNotFound)

	// The rejected update must not have mutated the persisted lens.
	got, _, err := m.Registry().Get("eng")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, namesOf(got.Reads))
}

func TestManager_UpdateLens_Replica(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)
	cloneLensRepo(t, m, "alpha", "alpha_clone")

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "alpha_clone"}},
	})
	require.ErrorIs(t, err, ErrReplicaInLens)

	got, _, err := m.Registry().Get("eng")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, namesOf(got.Reads), "rejected update leaves mounts intact")
}

func TestManager_UpdateLens_BranchUnknown(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta", Branch: "nope"}},
	})
	require.ErrorIs(t, err, ErrLensBranchUnknown)
}

func TestManager_UpdateLens_EmptyWrite(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{Name: "eng", Write: ""})
	require.ErrorIs(t, err, ErrLensWriteEmpty)
}

func TestManager_UpdateLens_DescriptionCap(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
		Description: strings.Repeat("x", MaxLensDescriptionBytes+1),
	})
	require.ErrorIs(t, err, ErrLensDescriptionTooLong)

	// Exactly at the cap is accepted.
	_, err = m.UpdateLens(context.Background(), Lens{
		Name: "eng", Write: "alpha", Reads: []LensRead{{Repo: "beta"}},
		Description: strings.Repeat("x", MaxLensDescriptionBytes),
	})
	require.NoError(t, err)
}

func TestManager_UpdateLens_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)
	makeLensRepo(t, m, "alpha")

	// Validation passes (alpha exists), but no lens row named "eng" exists yet.
	_, err := m.UpdateLens(context.Background(), Lens{Name: "eng", Write: "alpha"})
	require.ErrorIs(t, err, ErrLensNotFound)
}
