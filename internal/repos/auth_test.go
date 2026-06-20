package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
)

// TestAuthConfigFromSpec_BasicSplitsUserPassword is the regression test for the
// CreateRepoForm "basic" auth bug: the create API carries the basic credential
// as "user:password" in a single auth_token field, and authConfigFromSpec must
// split it so the immediate clone authenticates with a real username. Before the
// fix it produced BasicAuth{Username:"", Password:token}, which fails on every
// real host (GitHub/GitLab reject an empty username).
func TestAuthConfigFromSpec_BasicSplitsUserPassword(t *testing.T) {
	cfg := authConfigFromSpec(&OriginSpec{AuthMethod: "basic", AuthToken: "alice:s3cret"})
	require.Equal(t, "alice", cfg.User)
	require.Equal(t, "s3cret", cfg.Password)

	auth, err := resolveAuth(cfg, "")
	require.NoError(t, err)
	ba, ok := auth.(*githttp.BasicAuth)
	require.True(t, ok, "basic auth must resolve to BasicAuth")
	require.Equal(t, "alice", ba.Username, "username must come from the split token, not be empty")
	require.Equal(t, "s3cret", ba.Password)

	// A password containing ':' splits only on the first colon (SplitN/Cut).
	cfg2 := authConfigFromSpec(&OriginSpec{AuthMethod: "basic", AuthToken: "bob:p:a:ss"})
	require.Equal(t, "bob", cfg2.User)
	require.Equal(t, "p:a:ss", cfg2.Password)

	// token auth is unaffected — it never carries a username and must not split.
	tok := authConfigFromSpec(&OriginSpec{AuthMethod: "token", AuthToken: "ghp_x:y"})
	require.Equal(t, "", tok.User)
	require.Equal(t, "ghp_x:y", tok.Token)
}

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

// TestManager_ResolveAuth_NoneIsAnonymous verifies the explicit "none" auth
// method resolves to nil (anonymous) regardless of URL — even an SSH-style URL
// must NOT auto-promote to SSH when the user explicitly chose none. The local
// URLs require a permissive LocalOriginRoot, since ResolveAuth is also the gate
// that rejects local origins outside the configured root.
func TestManager_ResolveAuth_NoneIsAnonymous(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{LocalOriginRoot: "/srv/kb"},
		AgentBranch: "agent/test",
		KeyPath:     keyPath,
	})

	for _, url := range []string{
		"git@github.com:user/repo.git",
		"ssh://git@github.com/user/repo.git",
		"https://github.com/user/repo.git",
		"file:///srv/kb",
		"/srv/kb",
	} {
		auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, url)
		require.NoError(t, err, url)
		require.Nil(t, auth, url)
	}
}

// TestManager_ResolveAuth_GatesLocalOrigin verifies ResolveAuth is the clone
// boundary that enforces the local-origin policy: a local path outside the
// configured root (or with no root set) is rejected before any auth/clone,
// while network origins are never gated.
func TestManager_ResolveAuth_GatesLocalOrigin(t *testing.T) {
	// No root configured: local origins are disabled, network origins pass.
	off := New(context.Background(), Deps{Cfg: config.Config{}, AgentBranch: "agent/test"})
	_, err := off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, "/srv/kb")
	require.Error(t, err)
	_, err = off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "token", Token: "x"}, "https://github.com/u/r.git")
	require.NoError(t, err)

	// Root configured: in-root local origin passes, out-of-root is rejected.
	on := New(context.Background(), Deps{Cfg: config.Config{LocalOriginRoot: "/srv/kb"}, AgentBranch: "agent/test"})
	_, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, "/srv/kb/work")
	require.NoError(t, err)
	_, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, "/etc/passwd")
	require.Error(t, err)
}
