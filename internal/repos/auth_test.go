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
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
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
		Cfg:         config.Config{LocalOriginRoot: hostAbs(t, "/srv/kb")},
		AgentBranch: "agent/test",
		KeyPath:     keyPath,
	})

	for _, url := range []string{
		"git@github.com:user/repo.git",
		"ssh://git@github.com/user/repo.git",
		"https://github.com/user/repo.git",
		fileuri.New(hostAbs(t, "/srv/kb")),
		hostAbs(t, "/srv/kb"),
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
	_, err := off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, hostAbs(t, "/srv/kb"))
	require.Error(t, err)
	_, err = off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "token", Token: "x"}, "https://github.com/u/r.git")
	require.NoError(t, err)

	// Root configured: in-root local origin passes, out-of-root is rejected.
	on := New(context.Background(), Deps{Cfg: config.Config{LocalOriginRoot: hostAbs(t, "/srv/kb")}, AgentBranch: "agent/test"})
	_, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, filepath.Join(hostAbs(t, "/srv/kb"), "work"))
	require.NoError(t, err)
	_, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, hostAbs(t, "/etc/passwd"))
	require.Error(t, err)
}

// cert means the credential is the instance certificate on the knomit+https
// transport, so there is no go-git AuthMethod to return.
func TestResolveAuth_CertIsTransportLevel(t *testing.T) {
	auth, err := resolveAuth(config.RemoteAuthConfig{AuthMethod: "cert"}, "")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// For a fleet URL the method is FORCED to cert: "", "none" and "ssh" all
// resolve to no go-git auth (ssh with no key path would otherwise be an
// error, so the ssh row shows the forcing, not a pass-through), and an
// explicit token or basic is an error. The scheme is matched case-
// insensitively because go-git lowercases it.
func TestResolveAuthWithOrigin_FleetURLForcesCert(t *testing.T) {
	for _, url := range []string{"knomit+https://h:8443/git/kb", "KNOMIT+HTTPS://h:8443/git/kb"} {
		for _, method := range []string{"", "cert", "none", "ssh"} {
			auth, err := resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: method, SSHKey: ""}, "", url)
			require.NoError(t, err, "%s %q", url, method)
			require.Nil(t, auth, "%s %q", url, method)
		}
		for _, method := range []string{"token", "basic"} {
			_, err := resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: method, Token: "ghp_x", User: "u", Password: "p"}, "", url)
			require.Error(t, err, "%s %q", url, method)
			require.Contains(t, err.Error(), "instance certificate", "%s %q", url, method)
		}
	}
}

// cert on a non-fleet URL is NOT rejected here: that is validateURLAuth's job
// at the API edge (internal/web/helpers.go), which the wizard mirrors.
func TestResolveAuthWithOrigin_CertOnHTTPSIsLeftToTheEdge(t *testing.T) {
	auth, err := resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: "cert"}, "", "https://github.com/o/r.git")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// SABOTAGE (run against 800d35bc's code, before a comment-only amend): deleting remoteAuthFromRecord's fleet
// early return turns this red; disabling resolveAuthWithOrigin's fleet branch
// turns this and TestResolveAuthWithOrigin_FleetURLForcesCert red.
//
// A global [remote] token (KNOMIT_REMOTE_AUTH=token) must never be sent to a
// fleet peer: for a knomit+https origin the sync loop's auth ignores the
// global fallback entirely. Positive control: the same fallback on an https
// origin does produce the token.
func TestRemoteAuthFromRecord_FleetOriginIgnoresGlobalCredential(t *testing.T) {
	global := config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_global"}

	fleet := &store.Remote{URL: "knomit+https://h:8443/git/kb"}
	cfg := remoteAuthFromRecord(fleet, global)
	require.Empty(t, cfg.Token, "the global token was copied into a fleet origin's config")
	auth, err := resolveAuthWithOrigin(cfg, "", fleet.URL)
	require.NoError(t, err)
	require.Nil(t, auth)

	forge := &store.Remote{URL: "https://github.com/o/r.git"}
	auth, err = resolveAuthWithOrigin(remoteAuthFromRecord(forge, global), "", forge.URL)
	require.NoError(t, err)
	ba, ok := auth.(*githttp.BasicAuth)
	require.True(t, ok, "positive control: %T", auth)
	require.Equal(t, "ghp_global", ba.Password)

	// An EXPLICIT token on the fleet origin's own record is an error, not
	// silently dropped.
	_, err = resolveAuthWithOrigin(remoteAuthFromRecord(&store.Remote{URL: fleet.URL, AuthMethod: "token", AuthToken: "ghp_x"}, global), "", fleet.URL)
	require.Error(t, err)
}
