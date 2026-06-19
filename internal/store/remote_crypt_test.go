package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newCryptTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc
}

// A credential token must NEVER be persisted in plaintext. With no Crypt
// configured, SetRemote must refuse a non-empty token rather than store it
// unencrypted.
func TestSetRemote_RefusesPlaintextTokenWithoutCrypt(t *testing.T) {
	svc := newCryptTestService(t)

	err := svc.Remote().SetRemote("origin", "https://example.com/r.git", "main", "agent/test", 300, 300, "token", "ghp_supersecret")
	require.Error(t, err, "SetRemote must refuse to store a token without encryption configured")

	// Nothing must have been written — no row means no plaintext at rest.
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM remotes WHERE name = 'origin'`).Scan(&n))
	require.Zero(t, n, "no remotes row may be written when the token cannot be encrypted")
}

// With a Crypt configured, the token is encrypted at rest (the stored column is
// not the plaintext) and round-trips back through GetRemote.
func TestSetRemote_EncryptsTokenAtRest(t *testing.T) {
	svc := newCryptTestService(t)
	crypt, err := NewCrypt([]byte("test-key-material-for-hkdf"))
	require.NoError(t, err)
	svc.SetCrypt(crypt)

	const secret = "ghp_supersecret"
	require.NoError(t, svc.Remote().SetRemote("origin", "https://example.com/r.git", "main", "agent/test", 300, 300, "token", secret))

	var stored string
	require.NoError(t, svc.rh.db.QueryRow(`SELECT auth_token FROM remotes WHERE name = 'origin'`).Scan(&stored))
	require.NotEqual(t, secret, stored, "token must not be stored in plaintext")
	require.NotEmpty(t, stored)

	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, secret, got.AuthToken, "GetRemote must decrypt the stored token")
}

// An empty token needs no encryption, so SetRemote succeeds even without Crypt.
func TestSetRemote_EmptyTokenWithoutCryptOK(t *testing.T) {
	svc := newCryptTestService(t)

	require.NoError(t, svc.Remote().SetRemote("origin", "https://example.com/r.git", "main", "agent/test", 300, 300, "", ""))
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.AuthToken)
}
