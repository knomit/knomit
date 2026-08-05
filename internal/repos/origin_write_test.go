package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
