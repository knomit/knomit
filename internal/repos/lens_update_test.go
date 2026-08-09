package repos

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------- LensRegistry.Update (registry-layer mechanics) ----------

// uidsOf returns the read-mount repo uids of a lens, in stored (sorted) order.
func uidsOf(reads []LensRead) []string {
	out := make([]string, len(reads))
	for i, r := range reads {
		out[i] = r.RepoUID
	}
	return out
}

// wantMounts is the expected read-mount uid list for these repos: the registry
// stores mounts sorted by uid, and a real repo's uid is minted opaquely, so the
// expectation must be sorted here rather than written out by hand. Assertions
// built on it still check ORDER, not merely membership.
func wantMounts(ris ...*RepoInstance) []string {
	out := make([]string, len(ris))
	for i, ri := range ris {
		out[i] = ri.UID()
	}
	sort.Strings(out)
	return out
}

func TestLensRegistry_Update_ReplaceReads(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
	alpha := seedMember(t, m.Repos(), "alpha")
	beta := seedMember(t, m.Repos(), "beta")
	gamma := seedMember(t, m.Repos(), "gamma")

	_, err := reg.Create(Lens{Name: "eng", WriteUID: alpha, Reads: []LensRead{{RepoUID: beta}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// Wholesale replace: beta drops out, gamma comes in. normalize folds the
	// write repo (alpha) back in and sorts.
	updated, err := reg.Update(Lens{Name: "eng", WriteUID: alpha, Reads: []LensRead{{RepoUID: gamma}}, CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, []string{alpha, gamma}, uidsOf(updated.Reads))

	got, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{alpha, gamma}, uidsOf(got.Reads), "beta must be gone; gamma present")
}

func TestLensRegistry_Update_ChangeWrite(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
	alpha := seedMember(t, m.Repos(), "alpha")
	beta := seedMember(t, m.Repos(), "beta")
	gamma := seedMember(t, m.Repos(), "gamma")

	_, err := reg.Create(Lens{Name: "eng", WriteUID: alpha, Reads: []LensRead{{RepoUID: beta}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	updated, err := reg.Update(Lens{Name: "eng", WriteUID: gamma, Reads: []LensRead{{RepoUID: beta}}, CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, gamma, updated.WriteUID)
	require.Equal(t, []string{beta, gamma}, uidsOf(updated.Reads), "new write repo folds into reads")

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, gamma, got.WriteUID)
}

func TestLensRegistry_Update_DescriptionAndReadsRoundTrip(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
	alpha := seedMember(t, m.Repos(), "alpha")
	beta := seedMember(t, m.Repos(), "beta")

	_, err := reg.Create(Lens{Name: "eng", WriteUID: alpha, Reads: []LensRead{{RepoUID: beta}}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	// Same read set, new description: mounts survive verbatim, description updates.
	updated, err := reg.Update(Lens{Name: "eng", WriteUID: alpha, Reads: []LensRead{{RepoUID: beta}}, Description: "team kb", CreatedAt: 1, UpdatedAt: 2})
	require.NoError(t, err)
	require.Equal(t, "team kb", updated.Description)
	require.Equal(t, []string{alpha, beta}, uidsOf(updated.Reads))

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, "team kb", got.Description)
	require.Equal(t, []string{alpha, beta}, uidsOf(got.Reads))
}

func TestLensRegistry_Update_PreservesCreatedAt(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
	alpha := seedMember(t, m.Repos(), "alpha")

	_, err := reg.Create(Lens{Name: "eng", WriteUID: alpha, CreatedAt: 100, UpdatedAt: 100})
	require.NoError(t, err)

	_, err = reg.Update(Lens{Name: "eng", WriteUID: alpha, CreatedAt: 100, UpdatedAt: 200})
	require.NoError(t, err)

	got, _, err := reg.Get("eng")
	require.NoError(t, err)
	require.Equal(t, int64(100), got.CreatedAt, "created_at is immutable across Update")
	require.Equal(t, int64(200), got.UpdatedAt)
}

func TestLensRegistry_Update_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()
	alpha := seedMember(t, m.Repos(), "alpha")

	_, err := reg.Update(Lens{Name: "ghost", WriteUID: alpha, UpdatedAt: 1})
	require.ErrorIs(t, err, ErrLensNotFound)
	require.ErrorContains(t, err, "ghost")
}

func TestLensRegistry_Update_EmptyWrite(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.LensRegistry()

	_, err := reg.Update(Lens{Name: "eng", WriteUID: "", UpdatedAt: 1})
	require.ErrorIs(t, err, ErrLensWriteEmpty)
}

// ---------- Manager.UpdateLens (validation parity with CreateLens) ----------

// seedLens makes members and persists a starting lens via the validated path,
// returning the write member and the foreign read member.
func seedLens(t *testing.T, m *Manager) (alpha, beta *RepoInstance) {
	t.Helper()
	alpha = makeLensRepo(t, m, "alpha")
	beta = makeLensRepo(t, m, "beta")
	_, err := m.CreateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: beta.UID()}},
	})
	require.NoError(t, err)
	return alpha, beta
}

func TestManager_UpdateLens_PersistsAfterValidation(t *testing.T) {
	m := newLifecycleManager(t)
	alpha, _ := seedLens(t, m)
	gamma := makeLensRepo(t, m, "gamma")

	stored, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: gamma.UID()}}, Description: "d",
	})
	require.NoError(t, err)
	require.Equal(t, wantMounts(alpha, gamma), uidsOf(stored.Reads))
	require.Equal(t, "d", stored.Description)

	got, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantMounts(alpha, gamma), uidsOf(got.Reads), "beta replaced by gamma")
}

func TestManager_UpdateLens_UnknownMember(t *testing.T) {
	m := newLifecycleManager(t)
	alpha, beta := seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: "uid-ghost"}},
	})
	require.ErrorIs(t, err, ErrRepoNotFound)

	// The rejected update must not have mutated the persisted lens.
	got, _, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.Equal(t, wantMounts(alpha, beta), uidsOf(got.Reads))
}

func TestManager_UpdateLens_Replica(t *testing.T) {
	m := newLifecycleManager(t)
	alpha, beta := seedLens(t, m)
	clone := cloneLensRepo(t, m, "alpha", "alpha_clone")

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: clone.UID()}},
	})
	require.ErrorIs(t, err, ErrReplicaInLens)

	got, _, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.Equal(t, wantMounts(alpha, beta), uidsOf(got.Reads), "rejected update leaves mounts intact")
}

func TestManager_UpdateLens_BranchUnknown(t *testing.T) {
	m := newLifecycleManager(t)
	alpha, beta := seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: beta.UID(), Branch: "nope"}},
	})
	require.ErrorIs(t, err, ErrLensBranchUnknown)
}

func TestManager_UpdateLens_EmptyWrite(t *testing.T) {
	m := newLifecycleManager(t)
	seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{Name: "eng", WriteUID: ""})
	require.ErrorIs(t, err, ErrLensWriteEmpty)
}

func TestManager_UpdateLens_DescriptionCap(t *testing.T) {
	m := newLifecycleManager(t)
	alpha, beta := seedLens(t, m)

	_, err := m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: beta.UID()}},
		Description: strings.Repeat("x", MaxLensDescriptionBytes+1),
	})
	require.ErrorIs(t, err, ErrLensDescriptionTooLong)

	// Exactly at the cap is accepted.
	_, err = m.UpdateLens(context.Background(), Lens{
		Name: "eng", WriteUID: alpha.UID(), Reads: []LensRead{{RepoUID: beta.UID()}},
		Description: strings.Repeat("x", MaxLensDescriptionBytes),
	})
	require.NoError(t, err)
}

func TestManager_UpdateLens_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)
	alpha := makeLensRepo(t, m, "alpha")

	// Validation passes (alpha exists), but no lens row named "eng" exists yet.
	_, err := m.UpdateLens(context.Background(), Lens{Name: "eng", WriteUID: alpha.UID()})
	require.ErrorIs(t, err, ErrLensNotFound)
}
