package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestLegacyAuthReturnsDecryptedToken(t *testing.T) {
	svc := openTestService(t)
	crypt, err := NewCrypt([]byte("test-key-material"))
	require.NoError(t, err)
	svc.SetCrypt(crypt)
	require.NoError(t, svc.InitRepo(nil, "agent/test"))

	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/x.git", "main", "agent/test", 300, 300, "token", "s3cret"))

	method, token, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)
}

func TestLegacyAuthErrorsWhenTokenCannotBeDecrypted(t *testing.T) {
	svc := openTestService(t)
	crypt, err := NewCrypt([]byte("original-key"))
	require.NoError(t, err)
	svc.SetCrypt(crypt)
	require.NoError(t, svc.InitRepo(nil, "agent/test"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/x.git", "main", "agent/test", 300, 300, "token", "s3cret"))

	// Rotate the key: the stored ciphertext is now undecryptable. GetRemote
	// would hand back the ciphertext as "plaintext"; LegacyAuth must refuse.
	rotated, err := NewCrypt([]byte("different-key"))
	require.NoError(t, err)
	svc.SetCrypt(rotated)

	_, _, err = svc.Remote().LegacyAuth("origin")
	require.Error(t, err, "an undecryptable token must be an error, never ciphertext-as-plaintext")
}

func TestLegacyAuthErrorsWithNoCrypt(t *testing.T) {
	svc := openTestService(t)
	crypt, err := NewCrypt([]byte("original-key"))
	require.NoError(t, err)
	svc.SetCrypt(crypt)
	require.NoError(t, svc.InitRepo(nil, "agent/test"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/x.git", "main", "agent/test", 300, 300, "token", "s3cret"))

	svc.SetCrypt(nil) // agent key unreadable
	_, _, err = svc.Remote().LegacyAuth("origin")
	require.Error(t, err, "no crypt plus a stored token must be an error")
}

func TestLegacyAuthEmptyWhenNoTokenStored(t *testing.T) {
	svc := openTestService(t)
	require.NoError(t, svc.InitRepo(nil, "agent/test"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/x.git", "main", "agent/test", 300, 300, "", ""))

	method, token, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err, "nothing stored is not an error")
	require.Equal(t, "", method)
	require.Equal(t, "", token)
}

func TestClearAuthBlanksBothColumns(t *testing.T) {
	svc := openTestService(t)
	crypt, err := NewCrypt([]byte("k"))
	require.NoError(t, err)
	svc.SetCrypt(crypt)
	require.NoError(t, svc.InitRepo(nil, "agent/test"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/x.git", "main", "agent/test", 300, 300, "token", "s3cret"))

	require.NoError(t, svc.Remote().ClearAuth("origin"))

	method, token, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", method)
	require.Equal(t, "", token)

	// The rest of the remote must survive untouched.
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/x.git", rm.URL)
	require.Equal(t, "main", rm.Branch)
}
