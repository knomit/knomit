package repos

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// newTestManagerWithRepo returns a started Manager with one preset-ontology
// repo already registered under name, built from the package's real test
// harness (newTestManager + createRepo — see testhelpers_test.go) rather than
// a parallel one.
func newTestManagerWithRepo(t *testing.T, name string) (*Manager, *RepoInstance) {
	t.Helper()
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, name)
	return m, ri
}

// addTestRepo creates a second preset-ontology repo named name in an
// already-started manager. Thin wrapper over createRepo so rename tests read
// the same as the brief without a parallel harness.
func addTestRepo(t *testing.T, m *Manager, name string) *RepoInstance {
	t.Helper()
	return createRepo(t, m, name)
}

// TestRenameRepo_RekeysWithoutClosingTheStore is the whole point of choosing
// in-place re-key over Remove+Add: the SAME instance pointer must survive, with
// its store still open, because a rename changes a display string and must not
// cost a store close, an SSE drop and an index re-warm.
func TestRenameRepo_RekeysWithoutClosingTheStore(t *testing.T) {
	m, _ := newTestManagerWithRepo(t, "alpha")

	before := m.Get("alpha")
	require.NotNil(t, before)
	uid := before.UID()

	require.NoError(t, m.RenameRepo("alpha", "beta"))

	require.Nil(t, m.Get("alpha"), "the old name must no longer resolve")
	after := m.Get("beta")
	require.NotNil(t, after, "the new name must resolve")
	require.Same(t, before, after, "same instance — the store was never closed")
	require.Equal(t, "beta", after.Name(), "the instance reports its new name")
	require.Equal(t, uid, after.UID(), "uid is identity and never changes")
	require.Same(t, before, m.GetByUID(uid), "byUID still points at the live instance")

	// The store is still usable — this is what Remove+Add would have broken.
	require.NoError(t, after.WithRead(func(svc *store.Service) {}))
}

func TestRenameRepo_RejectsInvalidName(t *testing.T) {
	m, _ := newTestManagerWithRepo(t, "alpha")
	require.ErrorIs(t, m.RenameRepo("alpha", "Has Capitals"), ErrInvalidName)
	require.ErrorIs(t, m.RenameRepo("alpha", ""), ErrInvalidName)
	require.NotNil(t, m.Get("alpha"), "a rejected rename leaves the repo alone")
}

func TestRenameRepo_RejectsNameHeldByAnotherActiveRepo(t *testing.T) {
	m, _ := newTestManagerWithRepo(t, "alpha")
	addTestRepo(t, m, "beta")

	require.ErrorIs(t, m.RenameRepo("alpha", "beta"), ErrRepoExists)
	require.NotNil(t, m.Get("alpha"))
	require.NotNil(t, m.Get("beta"))
}

func TestRenameRepo_UnknownRepo(t *testing.T) {
	m, _ := newTestManagerWithRepo(t, "alpha")
	require.ErrorIs(t, m.RenameRepo("ghost", "beta"), ErrRepoNotFound)
}

// Renaming to the current name is a successful no-op, not a self-collision.
func TestRenameRepo_SameNameIsNoOp(t *testing.T) {
	m, _ := newTestManagerWithRepo(t, "alpha")
	before := m.Get("alpha")

	require.NoError(t, m.RenameRepo("alpha", "alpha"))
	require.Same(t, before, m.Get("alpha"))
}
