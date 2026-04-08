package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
)

// writeTestKey generates an ed25519 private key file at path.
func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0600))
}

// TestManager_ResolveAuth_SSHUsesManagerKeyPath verifies that ResolveAuth uses
// the manager's own key path for SSH, so callers never need to supply it.
func TestManager_ResolveAuth_SSHUsesManagerKeyPath(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{},
		AgentBranch: "agent/test",
		KeyPath:     keyPath,
	})

	auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh"}, "git@github.com:user/repo.git")
	require.NoError(t, err)
	require.NotNil(t, auth)
}

// TestManager_ResolveAuth_SSHAutoDetectedFromURL verifies that a git@ URL with
// no explicit auth method is auto-resolved as SSH using the manager key.
func TestManager_ResolveAuth_SSHAutoDetectedFromURL(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{},
		AgentBranch: "agent/test",
		KeyPath:     keyPath,
	})

	auth, err := m.ResolveAuth(config.RemoteAuthConfig{}, "git@github.com:user/repo.git")
	require.NoError(t, err)
	require.NotNil(t, auth)
}

// TestManager_ResolveAuth_SSHNoKeyFails verifies a clear error when the manager
// has no key path configured.
func TestManager_ResolveAuth_SSHNoKeyFails(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{},
		AgentBranch: "agent/test",
		KeyPath:     "",
	})

	_, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh"}, "git@github.com:user/repo.git")
	require.Error(t, err)
	require.Contains(t, err.Error(), "key path")
}

// TestManager_ResolveAuth_TokenNoKey verifies token auth works without a key path.
func TestManager_ResolveAuth_TokenNoKey(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{},
		AgentBranch: "agent/test",
		KeyPath:     "",
	})

	auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_abc"}, "https://github.com/user/repo.git")
	require.NoError(t, err)
	require.NotNil(t, auth)
}
