package repos

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"

	"knomit/internal/config"
)

// testHostKey returns a fresh ed25519 ssh.PublicKey.
func testHostKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := gossh.NewPublicKey(pub)
	require.NoError(t, err)
	return key
}

func testAddr(t *testing.T) net.Addr {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "192.0.2.10:22")
	require.NoError(t, err)
	return addr
}

// TestHostKeyCallback_TOFUThenPin is the core of the host-key fix (P0.7): the
// first contact with an unknown host is accepted and PINNED, and the same key
// verifies afterwards from the file alone.
func TestHostKeyCallback_TOFUThenPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	cb, err := hostKeyCallback(path)
	require.NoError(t, err)

	key := testHostKey(t)
	require.NoError(t, cb("git.example.com:22", testAddr(t), key),
		"unknown host must be accepted on first use")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "git.example.com", "first contact must pin the host")

	// A second callback (fresh, as a new process would build) trusts the pin.
	cb2, err := hostKeyCallback(path)
	require.NoError(t, err)
	require.NoError(t, cb2("git.example.com:22", testAddr(t), key),
		"a pinned host+key must verify without re-pinning")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(data), string(after), "verifying must not append a duplicate line")
}

// TestHostKeyCallback_RejectsChangedKey is the security assertion: once a host
// is pinned, a DIFFERENT key for it is rejected. This is the MITM /
// key-substitution case that ssh.InsecureIgnoreHostKey used to wave through.
func TestHostKeyCallback_RejectsChangedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	cb, err := hostKeyCallback(path)
	require.NoError(t, err)

	require.NoError(t, cb("git.example.com:22", testAddr(t), testHostKey(t)))

	err = cb("git.example.com:22", testAddr(t), testHostKey(t))
	require.Error(t, err, "a changed host key must be rejected, never silently re-pinned")
}

// TestHostKeyCallback_CreatesMissingFile: knownhosts.New errors on a missing
// file, so the callback must create it — otherwise the very first sync of a
// fresh install fails instead of taking the TOFU path.
func TestHostKeyCallback_CreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "known_hosts")
	_, err := hostKeyCallback(path)
	require.NoError(t, err)
	require.FileExists(t, path)
}

// TestResolveAuth_SSHVerifiesHostKeys asserts the wiring: SSH auth must never
// come back with an ignore-everything callback again.
func TestResolveAuth_SSHVerifiesHostKeys(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)
	knownHosts := filepath.Join(dir, "known_hosts")

	auth, err := resolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh", KnownHosts: knownHosts}, keyPath)
	require.NoError(t, err)
	pk, ok := auth.(*gitssh.PublicKeys)
	require.True(t, ok, "ssh auth must resolve to PublicKeys")
	require.NotNil(t, pk.HostKeyCallback, "ssh auth must carry a host-key callback")
	require.FileExists(t, knownHosts, "resolving ssh auth must prepare the known_hosts file")

	// The callback is the verifying one, not an ignore-all: it pins on first
	// contact and refuses a second key for the same host.
	key := testHostKey(t)
	require.NoError(t, pk.HostKeyCallback("git.example.com:22", testAddr(t), key))
	require.Error(t, pk.HostKeyCallback("git.example.com:22", testAddr(t), testHostKey(t)),
		"resolved callback must reject a changed host key")
}

// TestResolveAuth_SSHDefaultsKnownHostsBesideKey: with no configured path (zero
// -value config, as embedded callers and tests build), pins land next to the
// key they authenticate with rather than falling back to no verification.
func TestResolveAuth_SSHDefaultsKnownHostsBesideKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	auth, err := resolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh"}, keyPath)
	require.NoError(t, err)
	require.NotNil(t, auth)
	require.FileExists(t, filepath.Join(dir, "known_hosts"))
}
