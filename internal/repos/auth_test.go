package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/pki"
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

	_, auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh"}, "git@github.com:user/repo.git")
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

	_, auth, err := m.ResolveAuth(config.RemoteAuthConfig{}, "git@github.com:user/repo.git")
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

	_, _, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "ssh"}, "git@github.com:user/repo.git")
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

	_, auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_abc"}, "https://github.com/user/repo.git")
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
		_, auth, err := m.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, url)
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
	_, _, err := off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, hostAbs(t, "/srv/kb"))
	require.Error(t, err)
	_, _, err = off.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "token", Token: "x"}, "https://github.com/u/r.git")
	require.NoError(t, err)

	// Root configured: in-root local origin passes, out-of-root is rejected.
	on := New(context.Background(), Deps{Cfg: config.Config{LocalOriginRoot: hostAbs(t, "/srv/kb")}, AgentBranch: "agent/test"})
	_, _, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, filepath.Join(hostAbs(t, "/srv/kb"), "work"))
	require.NoError(t, err)
	_, _, err = on.ResolveAuth(config.RemoteAuthConfig{AuthMethod: "none"}, hostAbs(t, "/etc/passwd"))
	require.Error(t, err)
}

// cert means the credential is the instance certificate on the fleet
// transport, so there is no go-git AuthMethod to return.
func TestResolveAuth_CertIsTransportLevel(t *testing.T) {
	auth, err := resolveAuth(config.RemoteAuthConfig{AuthMethod: "cert"}, "")
	require.NoError(t, err)
	require.Nil(t, auth)
}

// The go-git URL is keyed on the stored METHOD, never on the URL: cert hands
// go-git the internal fleet form of the same https URL and no go-git auth;
// every other method the URL unchanged. A URL that already carries the
// internal scheme is refused for every method (it is never input and never
// stored), and cert on a non-https URL is refused.
func TestResolveAuthWithOrigin_MethodKeyedGitURL(t *testing.T) {
	gitURL, auth, err := resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: "cert"}, "", "https://h:8443/git/kb")
	require.NoError(t, err)
	require.Nil(t, auth)
	internalWant, err := pki.GitURL("https://h:8443/git/kb", true)
	require.NoError(t, err)
	require.Equal(t, internalWant, gitURL)

	gitURL, auth, err = resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_x"}, "", "https://h:8443/git/kb")
	require.NoError(t, err)
	require.NotNil(t, auth)
	require.Equal(t, "https://h:8443/git/kb", gitURL, "a non-cert origin goes to stock https unchanged")

	internal, err := pki.GitURL("https://h:8443/git/kb", true)
	require.NoError(t, err)
	scheme, rest, _ := strings.Cut(internal, "://")
	for _, method := range []string{"", "cert", "none", "token"} {
		for _, url := range []string{internal, strings.ToUpper(scheme) + "://" + rest} {
			_, _, err := resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: method, Token: "t"}, "", url)
			require.ErrorIs(t, err, pki.ErrUnsupportedOriginScheme, "%s %q", url, method)
		}
	}
	_, _, err = resolveAuthWithOrigin(config.RemoteAuthConfig{AuthMethod: "cert"}, "", "http://h:8443/git/kb")
	require.Error(t, err, "cert over plain http")
}

// SABOTAGE: keying remoteAuthFromRecord's early return back on the URL (or
// deleting it) turns this red.
//
// A global [remote] token (KNOMIT_REMOTE_AUTH=token) must never be sent to a
// fleet peer: for an origin stored with method cert the sync loop's auth
// ignores the global fallback entirely, whatever its URL looks like.
// Positive control: the same fallback on a token origin at the same URL does
// produce the token.
func TestRemoteAuthFromRecord_CertOriginIgnoresGlobalCredential(t *testing.T) {
	global := config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_global"}

	fleet := &store.Remote{URL: "https://h:8443/git/kb", AuthMethod: "cert"}
	cfg := remoteAuthFromRecord(fleet, global)
	require.Empty(t, cfg.Token, "the global token was copied into a fleet origin's config")
	require.Equal(t, "cert", cfg.AuthMethod)
	gitURL, auth, err := resolveAuthWithOrigin(cfg, "", fleet.URL)
	require.NoError(t, err)
	require.Nil(t, auth)
	want, err := pki.GitURL(fleet.URL, true)
	require.NoError(t, err)
	require.Equal(t, want, gitURL)

	forge := &store.Remote{URL: "https://h:8443/git/kb"}
	_, auth, err = resolveAuthWithOrigin(remoteAuthFromRecord(forge, global), "", forge.URL)
	require.NoError(t, err)
	ba, ok := auth.(*githttp.BasicAuth)
	require.True(t, ok, "positive control: %T", auth)
	require.Equal(t, "ghp_global", ba.Password)
}
