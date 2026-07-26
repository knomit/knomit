package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
)

// redactURL must strip secrets without destroying a URL's usability. An http(s)
// token can appear as EITHER the username or the password, so the whole
// userinfo goes. An ssh username is a LOGIN NAME, not a secret — stripping it
// would publish a source URL that no longer works.
func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"https user and password", "https://me:tok@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"https token as username", "https://TOKEN@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"bitbucket token user", "https://x-token-auth:tok@bitbucket.org/me/kb.git", "https://bitbucket.org/me/kb.git"},
		{"scp-like ssh untouched", "git@github.com:me/kb.git", "git@github.com:me/kb.git"},
		{"ssh url keeps login name", "ssh://git@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"ssh password dropped", "ssh://git:secret@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"anonymous knomit untouched", "http://localhost:19278/git/knomit-kb", "http://localhost:19278/git/knomit-kb"},
		{"local path untouched", "/srv/kb-mirror.git", "/srv/kb-mirror.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, redactURL(tc.in))
		})
	}
}

// A credential-bearing source URL must never reach the COMMITTED config —
// .knomit-okf.yaml is pushed, so a token there is a published token.
func TestRenderFiles_PublishSourceStripsCredentials(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	_ = kbDir
	head, err := kbRepo.Head()
	require.NoError(t, err)

	var out bytes.Buffer
	files, _, err := renderFiles(exportRequest{
		repo: kbRepo, dir: t.TempDir(), branch: head.Name().Short(), head: head.Hash(),
		source: "https://me:supersecret@github.com/me/kb.git", publishSource: true,
		ui: newUI(&out),
	})
	require.NoError(t, err)

	cfg := string(files[configFile])
	require.NotContains(t, cfg, "supersecret", "the committed config must not carry a token")
	require.Contains(t, cfg, "source: https://github.com/me/kb.git")
}

// A knomit instance needs no credentials, and a local path cannot take them.
// Returning nil for both is what keeps the common case untouched by this work.
func TestAuthFor_NilWhenNoCredentialsApply(t *testing.T) {
	m, err := authFor("http://localhost:19278/git/knomit-kb", authOpts{})
	require.NoError(t, err)
	require.Nil(t, m, "an anonymous knomit source must stay anonymous")

	m, err = authFor("/srv/kb-mirror.git", authOpts{token: "tok"})
	require.NoError(t, err)
	require.Nil(t, m, "a local path takes no credentials even if a token is set")
}

// GitHub, GitLab and Bitbucket all reject `Authorization: Bearer` for
// git-over-HTTPS; they want the token as the basic-auth PASSWORD. Using
// go-git's TokenAuth here would fail against every host we target.
func TestAuthFor_TokenIsBasicAuthNotBearer(t *testing.T) {
	m, err := authFor("https://github.com/me/kb.git", authOpts{token: "ghp_xyz"})
	require.NoError(t, err)

	basic, ok := m.(*githttp.BasicAuth)
	require.True(t, ok, "token auth must be BasicAuth, never TokenAuth (Bearer)")
	require.Equal(t, "ghp_xyz", basic.Password)
	require.Equal(t, defaultTokenUser, basic.Username)
}

// Bitbucket access tokens require the literal user "x-token-auth"; GitHub and
// GitLab ignore the field. Hence an override rather than a constant.
func TestAuthFor_UsernameOverride(t *testing.T) {
	m, err := authFor("https://bitbucket.org/me/kb.git",
		authOpts{token: "tok", username: "x-token-auth"})
	require.NoError(t, err)
	require.Equal(t, "x-token-auth", m.(*githttp.BasicAuth).Username)
}

func TestAuthOpts_Resolve(t *testing.T) {
	t.Run("token file is read and trimmed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "tok")
		require.NoError(t, os.WriteFile(p, []byte("ghp_fromfile\n"), 0o600))
		o := authOpts{tokenFile: p}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromfile", o.token)
	})

	t.Run("both token sources is an error", func(t *testing.T) {
		o := authOpts{token: "a", tokenFile: "b"}
		require.ErrorContains(t, o.resolve(), "mutually exclusive")
	})

	t.Run("environment fills an unset token", func(t *testing.T) {
		t.Setenv("KNOMIT_OKF_TOKEN", "ghp_fromenv")
		o := authOpts{}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromenv", o.token)
	})

	t.Run("flag beats environment", func(t *testing.T) {
		t.Setenv("KNOMIT_OKF_TOKEN", "ghp_fromenv")
		o := authOpts{token: "ghp_fromflag"}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromflag", o.token)
	})
}

// writeTestKey generates an unencrypted ed25519 private key in OpenSSH format.
func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := gossh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
}

// An explicit --ssh-key must win over everything, including a populated agent.
func TestSSHAuth_ExplicitKeyWins(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id_test")
	writeTestKey(t, key)

	m, err := authFor("git@github.com:me/kb.git", authOpts{sshKey: key})
	require.NoError(t, err)

	pk, ok := m.(*gitssh.PublicKeys)
	require.True(t, ok, "--ssh-key must produce PublicKeys")
	require.Equal(t, "git", pk.User, "the ssh user comes from the URL")
}

// The default-identity fallback is what makes a repo that clones fine with
// plain `git` work here too. It is reachable ONLY if an empty agent is treated
// as "no credentials", so this test pins that behaviour.
func TestSSHAuth_FallsBackToDefaultIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("SSH_AUTH_SOCK", "") // no agent
	writeTestKey(t, filepath.Join(home, ".ssh", "id_ed25519"))

	m, err := authFor("git@github.com:me/kb.git", authOpts{})
	require.NoError(t, err)
	require.IsType(t, &gitssh.PublicKeys{}, m, "~/.ssh/id_ed25519 must be found")
}

// The error has to say what was tried, or the user cannot act on it.
func TestSSHAuth_ErrorListsWhatWasTried(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := authFor("git@github.com:me/kb.git", authOpts{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--ssh-key")
	require.Contains(t, err.Error(), "id_ed25519")
	require.Contains(t, err.Error(), "ssh-add", "the error must carry a fix")
}

// A bad key path fails loudly rather than silently falling through to another
// credential: the user asked for THIS key.
func TestSSHAuth_ExplicitKeyErrorIsNotSwallowed(t *testing.T) {
	_, err := authFor("git@github.com:me/kb.git",
		authOpts{sshKey: filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "missing")
}
