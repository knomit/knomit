package control

import (
	"path/filepath"
	"testing"

	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

func openTestOrigins(t *testing.T, crypt *model.Crypt) (*Registry, *Origins) {
	t.Helper()
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	o := OpenOrigins(r.DB(), crypt)
	return r, o
}

func testCrypt(t *testing.T) *model.Crypt {
	t.Helper()
	c, err := model.NewCrypt([]byte("test-key-material-not-a-real-ssh-key"))
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
	want.Mode = OriginModeSync // Get always fills Mode; an unset Mode on Set means sync.
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
// Since the store dropped its own credential path this is the ONLY place a
// credential can be written, so this refusal is the whole guarantee.
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

	require.NoError(t, o.SetBranch("u1", "u", "release"))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "release", got.Branch)
	require.Equal(t, "s3cret", got.AuthToken)
}

// SetBranch is a compare-and-set on the url: a branch vetted against one
// origin is never written onto another one that replaced it, nor onto a
// removed one.
//
// SABOTAGE: drop `AND url = ?` from the UPDATE -> the write lands on R2 -> red.
func TestOrigins_SetBranchRefusesAChangedURL(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "https://r2.test/kb.git", Branch: "main"}))

	require.ErrorIs(t, o.SetBranch("u1", "https://r1.test/kb.git", "ybranch"), ErrOriginChanged)
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "https://r2.test/kb.git", got.URL)
	require.Equal(t, "main", got.Branch, "a branch vetted against R1 must not land on R2")

	require.NoError(t, o.Delete("u1"))
	require.ErrorIs(t, o.SetBranch("u1", "https://r2.test/kb.git", "ybranch"), ErrOriginChanged)
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

// Mode round-trips through the repo_subscriptions presence table: Set writes a
// row when Mode is subscribe, Get reads it back through the LEFT JOIN, and
// re-Setting without a mode removes the row rather than leaving it stale.
func TestOrigins_ModeRoundTripsThroughPresenceTable(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "sub", State: StateActive, Profile: "code", CreatedAt: 1}))

	require.NoError(t, o.Set("u1", Origin{URL: "https://example.com/kb.git", Branch: "main", Mode: OriginModeSubscribe}))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, OriginModeSubscribe, got.Mode)

	// Re-setting without a mode means sync: the presence row is removed.
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.com/kb.git", Branch: "main"}))
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, OriginModeSync, got.Mode)

	var n int
	require.NoError(t, r.DB().QueryRow(`SELECT count(*) FROM repo_subscriptions WHERE repo_uid = ?`, "u1").Scan(&n))
	require.Equal(t, 0, n)
}

// An unknown Mode is refused, and because the check runs INSIDE Set's
// transaction — after the repo_origins upsert has already executed — the whole
// call rolls back rather than half-landing. That is a new contract worth
// pinning: before subscriptions existed, Set always wrote the origin row.
func TestOrigins_UnknownModeRollsBackTheWholeSet(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))

	// Record a subscription first, so the rollback has BOTH halves of the
	// transaction to undo, not just the upsert.
	require.NoError(t, o.Set("u1", Origin{URL: "https://first.test/kb.git", Branch: "main", Mode: OriginModeSubscribe}))

	err := o.Set("u1", Origin{URL: "https://second.test/kb.git", Branch: "main", Mode: "bogus"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus", "the error must name the mode it refused")

	got, gerr := o.Get("u1")
	require.NoError(t, gerr)
	require.NotNil(t, got)
	require.Equal(t, "https://first.test/kb.git", got.URL, "the repo_origins upsert must have rolled back")
	require.Equal(t, OriginModeSubscribe, got.Mode, "a refused Set must not disturb the subscription")
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

func newLinkableSecret(t *testing.T, s *Secrets, name string) string {
	t.Helper()
	meta, err := s.Create(SecretInput{Name: name, Type: SecretTypeToken, Hosts: []string{"example.test"},
		Value: SecretValue{Token: "t"}}, testNow)
	require.NoError(t, err)
	return meta.ID
}

func TestOrigins_SecretIDRoundTrips(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	id := newLinkableSecret(t, s, "pat")

	want := Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token", Mode: OriginModeSync, SecretID: id}
	require.NoError(t, o.Set("u1", want))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, want, *got)
}

func TestOrigins_SetWithoutSecretIDUnlinks(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	id := newLinkableSecret(t, s, "pat")
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token", SecretID: id}))
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token"}))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Empty(t, got.SecretID)
	linked, err := s.LinkedRepos(id)
	require.NoError(t, err)
	require.Empty(t, linked)
}

func TestOrigins_SetCredentialKeepsModeAndBranch(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	url := "https://example.test/kb.git"
	require.NoError(t, o.Set("u1", Origin{URL: url, Branch: "trunk", AuthMethod: "token", AuthToken: "old", Mode: OriginModeSubscribe}))

	id := newLinkableSecret(t, s, "pat")
	require.NoError(t, o.SetCredential("u1", url, "token", "", id))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, Origin{URL: url, Branch: "trunk", AuthMethod: "token", Mode: OriginModeSubscribe, SecretID: id}, *got)

	require.NoError(t, o.SetCredential("u1", url, "basic", "u:p", ""))
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, Origin{URL: url, Branch: "trunk", AuthMethod: "basic", AuthToken: "u:p", Mode: OriginModeSubscribe}, *got)

	var raw string
	require.NoError(t, r.DB().QueryRow(`SELECT auth_token FROM repo_origins WHERE repo_uid = 'u1'`).Scan(&raw))
	require.NotEqual(t, "u:p", raw, "encrypted at rest")

	_, o2, _ := openTestSecrets(t, nil)
	require.Error(t, o2.SetCredential("u1", url, "token", "x", ""), "no crypt, no stored token")
}

func TestOrigins_SetCredentialCASRefusesChangedURL(t *testing.T) {
	r, o, _ := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/new.git", Branch: "main", AuthMethod: "token", AuthToken: "keep"}))

	err := o.SetCredential("u1", "https://example.test/old.git", "token", "new", "")
	require.ErrorIs(t, err, ErrOriginChanged)
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "keep", got.AuthToken, "nothing written")
	require.ErrorIs(t, o.SetCredential("missing", "https://x/y", "token", "", ""), ErrOriginChanged)
}

func TestOrigins_DeleteDropsLink(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	id := newLinkableSecret(t, s, "pat")
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token", SecretID: id}))
	require.NoError(t, o.Delete("u1"))
	require.NoError(t, s.Delete(id), "the link went with the origin")
}

func TestOrigins_LinkToMissingSecretFails(t *testing.T) {
	r, o, _ := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))
	err := o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token", SecretID: "nope"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "FOREIGN KEY")
	got, gerr := o.Get("u1")
	require.NoError(t, gerr)
	require.Nil(t, got, "the whole Set rolled back")
	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "main", AuthMethod: "token"}))
	err = o.SetCredential("u1", "https://example.test/kb.git", "token", "", "nope")
	require.Error(t, err)
}

func TestOrigins_ReplaceAndRestore(t *testing.T) {
	r, o, _ := openTestSecrets(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))

	// No previous row: Replace reports nil and Restore deletes.
	written := Origin{URL: "https://example.test/new.git", Branch: "main", AuthMethod: "token", AuthToken: "w"}
	prev, err := o.Replace("u1", written)
	require.NoError(t, err)
	require.Nil(t, prev)
	require.NoError(t, o.Restore("u1", written, prev))
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Nil(t, got)

	old := Origin{URL: "https://example.test/old.git", Branch: "main", AuthMethod: "token", AuthToken: "o", Mode: OriginModeSubscribe}
	require.NoError(t, o.Set("u1", old))
	prev, err = o.Replace("u1", written)
	require.NoError(t, err)
	require.Equal(t, old, *prev)

	// Unchanged since the write: the previous row comes back whole.
	require.NoError(t, o.Restore("u1", written, prev))
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, old, *got)

	// A credential changed after the write was vetted against the WRITTEN
	// URL: across URLs it is never carried to the previous one.
	_, err = o.Replace("u1", written)
	require.NoError(t, err)
	require.NoError(t, o.SetCredential("u1", written.URL, "basic", "u:p", ""))
	require.NoError(t, o.Restore("u1", written, prev))
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, old, *got, "another host's credential is never restored onto the previous URL")

	// Same URL (a swap that keeps the origin): the newer credential is kept.
	sameURL := Origin{URL: old.URL, Branch: "main", AuthMethod: "token", AuthToken: "w", Mode: OriginModeSubscribe}
	prev, err = o.Replace("u1", sameURL)
	require.NoError(t, err)
	require.NoError(t, o.SetCredential("u1", old.URL, "basic", "u:p", ""))
	require.NoError(t, o.Restore("u1", sameURL, prev))
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, Origin{URL: old.URL, Branch: "main", AuthMethod: "basic", AuthToken: "u:p", Mode: OriginModeSubscribe}, *got)
}
