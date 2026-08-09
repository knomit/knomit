package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

func openTestOrigins(t *testing.T, crypt *store.Crypt) (*Registry, *Origins) {
	t.Helper()
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	o, err := OpenOrigins(r.DB(), crypt)
	require.NoError(t, err)
	return r, o
}

func testCrypt(t *testing.T) *store.Crypt {
	t.Helper()
	c, err := store.NewCrypt([]byte("test-key-material-not-a-real-ssh-key"))
	require.NoError(t, err)
	return c
}

func TestOrigins_SetAndGetRoundTrips(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))

	want := Origin{URL: "https://example.test/kb.git", Branch: "master", AuthMethod: "token", AuthToken: "s3cret"}
	require.NoError(t, o.Set("u1", want))

	got, err := o.Get("u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want, *got, "token round-trips as plaintext")
}

// A repo with no origin is an ordinary state, not an error — it matches
// GetRemote's existing nil,nil contract.
func TestOrigins_GetAbsentIsNilNil(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Nil(t, got)
}

// The token is never at rest in plaintext.
func TestOrigins_TokenEncryptedAtRest(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "u", Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}))

	var stored string
	require.NoError(t, r.DB().QueryRow(`SELECT auth_token FROM repo_origins WHERE repo_uid = 'u1'`).Scan(&stored))
	require.NotEmpty(t, stored)
	require.NotContains(t, stored, "s3cret")
}

// No Crypt means no credential storage — refuse rather than persist plaintext.
// Mirrors SetRemote's refusal.
func TestOrigins_RefusesTokenWithoutCrypt(t *testing.T) {
	r, o := openTestOrigins(t, nil)
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))

	err := o.Set("u1", Origin{URL: "u", Branch: "main", AuthMethod: "token", AuthToken: "s3cret"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "encryption unavailable")

	// An origin with NO credential still stores fine without a Crypt.
	require.NoError(t, o.Set("u1", Origin{URL: "u", Branch: "main"}))
}

func TestOrigins_SetBranchPreservesAuth(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "u", Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}))

	require.NoError(t, o.SetBranch("u1", "release"))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "release", got.Branch)
	require.Equal(t, "s3cret", got.AuthToken)
}

// Origin uniqueness is now one indexed query instead of opening every repo.
// Archived repos do not hold an origin claim, matching how they release names.
func TestOrigins_ActiveRepoWithURL(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "https://x.test/kb.git", Branch: "main"}))

	name, err := o.ActiveRepoWithURL("https://x.test/kb.git")
	require.NoError(t, err)
	require.Equal(t, "alpha", name)

	name, err = o.ActiveRepoWithURL("https://other.test/kb.git")
	require.NoError(t, err)
	require.Empty(t, name)

	require.NoError(t, r.SetState("u1", StateArchived, 9))
	name, err = o.ActiveRepoWithURL("https://x.test/kb.git")
	require.NoError(t, err)
	require.Empty(t, name, "an archived repo releases its origin claim")
}

// Purging a repo destroys its stored credential with it.
func TestOrigins_CascadesOnRepoDelete(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "u", Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}))

	require.NoError(t, r.Delete("u1"))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Nil(t, got)
}
