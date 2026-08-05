package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newCredentialManager builds a started Manager whose registry HAS a Crypt.
//
// The key file is not decoration: SetOriginCredential refuses to store a token
// without one (never plaintext), so a test that omitted it would fail on that
// refusal rather than on the behaviour under test, and would keep failing after
// the behaviour was correct.
func newCredentialManager(t *testing.T, home, originRoot string) *Manager {
	t.Helper()
	keyPath := filepath.Join(home, "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))
	m := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = originRoot
		d.KeyPath = keyPath
	})
	require.NoError(t, m.Start())
	return m
}

func TestSetOriginWritesControlDBBeforeStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")

	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	// control.db holds the credential...
	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)

	// ...and the store holds the wiring but NOT the secret.
	svc := testService(t, m.Get("work"))
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm, "the store must not keep the auth method")
	require.Equal(t, "", sTok, "the store must not keep the token")

	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, url, rm.URL)

	// The registry row carries the origin too — a repo whose database is lost
	// has to be re-clonable from control.db alone.
	rec, found, err := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, url, rec.OriginURL)
	require.Equal(t, "main", rec.OriginBranch)
}

func TestClearOriginForgetsTheCredential(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	require.NoError(t, m.ClearOrigin(ctx, "work"))

	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "", method)
	require.Equal(t, "", token)

	rec, found, err := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found, "clearing an origin must not unregister the repo")
	require.Equal(t, "", rec.OriginURL)

	// The store is unwired too, so nothing keeps syncing to the origin the
	// user just disconnected.
	svc := testService(t, m.Get("work"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, rm, "the store must not keep a remote control.db no longer names")
}

// TestSetOriginKeepsCredentialWhenStoreWriteFails is the ORDERING test, and it
// is the only evidence that the rule is enforced rather than merely asserted in
// a comment. Every other test here pins the end state of a run where both steps
// succeeded, which the reverse order would satisfy just as well.
//
// It forces step 2 to fail — the instance has no store handle, so Acquire
// returns ErrStoreUnavailable — and then asserts the credential and the origin
// row are in control.db anyway. That is precisely the state a crash between the
// two steps leaves behind, and the state the next boot repairs by
// materializing. Written store-first instead, this run would record nothing
// recoverable: the origin would exist nowhere but in the caller's arguments.
func TestSetOriginKeepsCredentialWhenStoreWriteFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	m := newCredentialManager(t, home, "")

	// A registered repo whose store cannot be acquired. NewTestInstanceWithDeps
	// with no Svc leaves the handle nil, which is exactly what Acquire reports
	// as ErrStoreUnavailable — no real store to tear down, and deterministic.
	m.Set("work", NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "work", AgentBranch: "machine/test",
	}))

	const url = "https://example.invalid/acme/kb.git"
	err := m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300)
	require.Error(t, err, "a failed store write must be reported, not swallowed")
	require.ErrorIs(t, err, ErrStoreUnavailable)

	// ...and control.db kept BOTH halves regardless.
	method, token, cerr := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, cerr)
	require.Equal(t, "token", method, "the credential must survive a failed store write")
	require.Equal(t, "s3cret", token, "the credential must survive a failed store write")

	rec, found, rerr := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, rerr)
	require.True(t, found)
	require.Equal(t, url, rec.OriginURL, "the origin must survive a failed store write")
	require.Equal(t, "main", rec.OriginBranch)
}

// TestClearOriginForgetsCredentialEvenWhenStoreUnwireFails is the mirror of
// TestSetOriginKeepsCredentialWhenStoreWriteFails, and it exists for the mirror
// reason. ClearOrigin's doc comment claims that clearing control.db first means
// a crash leaves only stale wiring the next boot removes — whereas clearing the
// store first would leave control.db still naming the origin, and the next boot
// would dutifully re-materialize the origin the user just disconnected. That is
// the origin-resurrection failure, and until this test it was asserted by the
// comment alone: reordering the two steps passed the whole suite.
//
// Same lever as the SetOrigin case — an instance with no store handle, so step
// 2 fails at Acquire — and the assertion is that control.db has ALREADY
// forgotten both halves by then.
//
// The registry is seeded directly rather than through SetOrigin, so a
// regression in SetOrigin cannot make this test vacuous.
func TestClearOriginForgetsCredentialEvenWhenStoreUnwireFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	m := newCredentialManager(t, home, "")

	const url = "https://example.invalid/acme/kb.git"
	reg := m.RepoRegistry()
	require.NoError(t, reg.Upsert(RepoRecord{
		Name: "work", State: RepoActive, CreatedAt: time.Now().UTC(),
		OriginURL: url, OriginBranch: "main",
	}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))

	m.Set("work", NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "work", AgentBranch: "machine/test",
	}))

	err := m.ClearOrigin(ctx, "work")
	require.Error(t, err, "a failed store unwire must be reported, not swallowed")
	require.ErrorIs(t, err, ErrStoreUnavailable)

	// ...and control.db had already forgotten the origin before it tried.
	method, token, cerr := reg.OriginCredential("work")
	require.NoError(t, cerr)
	require.Equal(t, "", method, "a revoked credential must not outlive the disconnect")
	require.Equal(t, "", token, "a revoked credential must not outlive the disconnect")

	rec, found, rerr := reg.ActiveRecord("work")
	require.NoError(t, rerr)
	require.True(t, found, "clearing an origin must not unregister the repo")
	require.Equal(t, "", rec.OriginURL,
		"control.db still naming the origin is how a disconnected origin resurrects itself at the next boot")
	require.Equal(t, "", rec.OriginBranch)
}

// TestCreateCloneRecordsTheCredentialInControlDB covers the one origin write
// that cannot go through SetOrigin: initClone runs before the RepoInstance
// exists. Create's registry write-through is what carries the credential
// instead, and the store must come out of the clone with empty auth columns.
func TestCreateCloneRecordsTheCredentialInControlDB(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"},
	}, nil)
	require.NoError(t, err)

	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)

	svc := testService(t, m.Get("work"))
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm, "the clone must not leave the auth method in the store")
	require.Equal(t, "", sTok, "the clone must not leave the token in the store")
}

// TestRebuildFromOriginKeepsTheStoredCredential guards the one path where
// Create runs against a registry row that ALREADY holds a credential:
// Manager.Start rebuilds a registered repo whose database is gone by
// re-entering Create with a spec carrying the URL and branch but no
// credential. A write-through that wrote that empty credential through
// unconditionally would erase the only surviving copy at exactly the moment
// the repo is being recovered from it.
func TestRebuildFromOriginKeepsTheStoredCredential(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	m := newCredentialManager(t, home, root)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, m.Close())

	// Lose the repo's database — control.db is now the only record of it.
	require.NoError(t, os.Remove(filepath.Join(home, "repos", "work.db")))

	m2 := newCredentialManager(t, home, root)
	require.NotNil(t, m2.Get("work"), "the repo should have been rebuilt from its recorded origin")

	_, token, err := m2.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "s3cret", token, "a rebuild that brings no credential must not erase the stored one")
}
